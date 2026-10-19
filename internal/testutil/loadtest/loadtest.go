// Package loadtest provides load testing infrastructure for the proxy.
package loadtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/earlgrey/piproxy/internal/httpapi/dataplane"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	"github.com/earlgrey/piproxy/internal/httpapi/openai"
	"github.com/earlgrey/piproxy/internal/logging"
	"github.com/earlgrey/piproxy/internal/providers"
	"github.com/earlgrey/piproxy/internal/ratelimit"
	"github.com/earlgrey/piproxy/internal/rewrite"
	"github.com/earlgrey/piproxy/internal/upstream"

	"github.com/rs/zerolog"
)

// ErrLocalListenerUnavailable indicates this environment cannot bind local
// listeners needed by httptest.Server.
var ErrLocalListenerUnavailable = errors.New("local test listener unavailable")

// IsLocalListenerUnavailable reports whether err came from local listener setup.
func IsLocalListenerUnavailable(err error) bool {
	return errors.Is(err, ErrLocalListenerUnavailable)
}

// Result holds the outcome of a single request.
type Result struct {
	Duration time.Duration
	Status   int
	Bytes    int
	Error    error
}

// Config configures the load test.
type Config struct {
	Concurrency    int           // number of concurrent workers
	TotalRequests  int           // total requests to send
	UpstreamDelay  time.Duration // simulated upstream processing time
	BodySize       int           // request body size in bytes
	Streaming      bool          // use streaming endpoint
	RequestTimeout time.Duration // per-request timeout
}

// DefaultConfig returns a sensible default configuration.
func DefaultConfig() Config {
	return Config{
		Concurrency:    10,
		TotalRequests:  100,
		UpstreamDelay:  200 * time.Millisecond,
		BodySize:       512,
		RequestTimeout: 30 * time.Second,
	}
}

// Suite holds the load test environment.
type Suite struct {
	Config       Config
	MockUpstream *httptest.Server
	ProxyServer  *httptest.Server
	Logger       zerolog.Logger
}

// NewSuite creates a load test suite with mock upstream and proxy servers.
func NewSuite(cfg Config) (*Suite, error) {
	logger := logging.NewNopLogger()

	// 1. Create mock upstream server.
	mockUpstream, err := newMockUpstream(cfg.UpstreamDelay, cfg.Streaming)
	if err != nil {
		return nil, err
	}

	// 2. Set up provider registry with mock upstream URL.
	registry := providers.NewRegistry()
	mockProvider := &providers.Provider{
		Type:               providers.ProviderOpenAI,
		Name:               "MockOpenAI",
		PublicBasePath:     "/openai",
		UpstreamBaseURL:    mockUpstream.URL,
		AllowedPaths:       []string{"/v1/chat/completions", "/v1/models"},
		AllowPrivateEgress: true,
		Streaming:          true,
		Timeout:            5 * time.Minute,
		MaxBodyBytes:       20 * 1024 * 1024,
		Enabled:            true,
	}
	if err := registry.RegisterProvider(mockProvider); err != nil {
		mockUpstream.Close()
		return nil, fmt.Errorf("register mock provider: %w", err)
	}

	// 3. Create upstream client.
	upstreamClient := upstream.NewClient(upstream.Config{
		Timeout:         cfg.RequestTimeout,
		MaxConnsPerHost: 200,
		MaxIdleConns:    200,
	})

	// 4. Create request rewriter.
	requestRewriter := rewrite.NewRequestRewriter(registry)

	// 5. Create IP rate limiter (generous limit).
	ipLimiter := ratelimit.NewMiddleware(ratelimit.NewMemoryStore(), 10000, 1*time.Minute)

	// 6. Create a focused data-plane router. The production public router fails
	// closed when auth is unavailable; this load harness intentionally bypasses
	// auth so it measures forwarding/streaming behavior against the mock
	// provider rather than auth backend setup.
	chatHandler := openai.NewChatHandler(upstreamClient, requestRewriter, registry, dataplane.Services{})
	router := chi.NewRouter()
	router.Use(middleware.RequestID)
	router.Use(middleware.RequestLogging(logger))
	router.Use(ipLimiter.Wrap)
	router.Use(middleware.BodySize(20 * 1024 * 1024))
	router.Post("/openai/v1/chat/completions", chatHandler.Handle)

	// 7. Create proxy server.
	proxyServer, err := newLocalServer(router)
	if err != nil {
		mockUpstream.Close()
		return nil, err
	}

	return &Suite{
		Config:       cfg,
		MockUpstream: mockUpstream,
		ProxyServer:  proxyServer,
		Logger:       logger,
	}, nil
}

// Close shuts down the suite.
func (s *Suite) Close() {
	s.ProxyServer.Close()
	s.MockUpstream.Close()
}

// RunLoadTest executes the load test and returns results.
func (s *Suite) RunLoadTest(ctx context.Context) []Result {
	results := make([]Result, s.Config.TotalRequests)
	resultCh := make(chan Result, s.Config.TotalRequests)
	workCh := make(chan int, s.Config.TotalRequests)

	// Fill work queue.
	for i := 0; i < s.Config.TotalRequests; i++ {
		workCh <- i
	}
	close(workCh)

	// Determine endpoint and body.
	var endpoint string
	if s.Config.Streaming {
		endpoint = s.ProxyServer.URL + "/openai/v1/chat/completions?stream=true"
	} else {
		endpoint = s.ProxyServer.URL + "/openai/v1/chat/completions"
	}

	// Build a realistic chat completion request body.
	bodyPayload := fmt.Sprintf(`{
		"model": "gpt-4o",
		"messages": [{"role": "user", "content": "%s"}],
		"max_tokens": 50
	}`, padString("Hello, tell me a story about AI.", s.Config.BodySize))
	bodyBytes := []byte(bodyPayload)

	// Start workers.
	var wg sync.WaitGroup
	for w := 0; w < s.Config.Concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := &http.Client{
				Timeout: s.Config.RequestTimeout + 5*time.Second,
				Transport: &http.Transport{
					MaxIdleConns:        100,
					MaxConnsPerHost:     100,
					MaxIdleConnsPerHost: 100,
				},
			}

			for range workCh {
				start := time.Now()
				req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(bodyBytes))
				if err != nil {
					resultCh <- Result{Error: fmt.Errorf("create request: %w", err)}
					continue
				}
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Corp-Proxy-Key", "test-key")
				req.ContentLength = int64(len(bodyBytes))

				resp, err := client.Do(req)
				duration := time.Since(start)
				if err != nil {
					resultCh <- Result{Duration: duration, Error: err}
					continue
				}

				// Read body to completion.
				var buf [4096]byte
				totalBytes := 0
				for {
					n, err := resp.Body.Read(buf[:])
					totalBytes += n
					if err != nil {
						break
					}
				}
				resp.Body.Close()

				resultCh <- Result{
					Duration: duration,
					Status:   resp.StatusCode,
					Bytes:    totalBytes,
				}
			}
		}()
	}

	// Wait for all workers.
	wg.Wait()
	close(resultCh)

	// Collect results.
	i := 0
	for r := range resultCh {
		results[i] = r
		i++
	}

	return results
}

// AnalyzeResults computes statistics from load test results.
func AnalyzeResults(results []Result) map[string]interface{} {
	if len(results) == 0 {
		return map[string]interface{}{
			"error": "no results",
		}
	}

	var totalDuration time.Duration
	var successCount, errorCount int
	durations := make([]float64, 0, len(results))
	statusCounts := make(map[int]int)

	for _, r := range results {
		totalDuration += r.Duration
		durations = append(durations, float64(r.Duration.Microseconds()))
		statusCounts[r.Status]++
		if r.Error != nil || r.Status >= 500 {
			errorCount++
		} else {
			successCount++
		}
	}

	// Sort for percentiles.
	sortFloats(durations)

	p50 := percentile(durations, 50)
	p90 := percentile(durations, 90)
	p95 := percentile(durations, 95)
	p99 := percentile(durations, 99)

	totalSec := totalDuration.Seconds()
	var rps float64
	if totalSec > 0 {
		rps = float64(len(results)) / totalSec
	}

	return map[string]interface{}{
		"total_requests":   len(results),
		"success_count":    successCount,
		"error_count":      errorCount,
		"error_rate":       float64(errorCount) / float64(len(results)),
		"requests_per_sec": rps,
		"status_codes":     statusCounts,
		"latency": map[string]float64{
			"p50_ms": p50 / 1000,
			"p90_ms": p90 / 1000,
			"p95_ms": p95 / 1000,
			"p99_ms": p99 / 1000,
			"avg_ms": (float64(totalDuration.Microseconds()) / float64(len(results))) / 1000,
		},
	}
}

func percentile(sorted []float64, p int) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := (p * len(sorted)) / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func sortFloats(s []float64) {
	n := len(s)
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if s[i] > s[j] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
}

// newMockUpstream creates a mock upstream server that simulates an LLM provider.
func newMockUpstream(delay time.Duration, streaming bool) (*httptest.Server, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		// Simulate processing delay.
		if delay > 0 {
			time.Sleep(delay)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-test",
			"object": "chat.completion",
			"created": 1717200000,
			"model": "gpt-4o",
			"choices": [{"index": 0, "message": {"role": "assistant", "content": "Hello, this is a test response."}, "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 50, "completion_tokens": 30, "total_tokens": 80}
		}`))
	})
	return newLocalServer(mux)
}

func newLocalServer(handler http.Handler) (*httptest.Server, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLocalListenerUnavailable, err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener = listener
	server.Start()
	return server, nil
}

// padString extends s to at least minLen bytes by repeating padding.
func padString(s string, minLen int) string {
	if len(s) >= minLen {
		return s
	}
	return s + strings.Repeat(" x", (minLen-len(s)+1)/2)
}
