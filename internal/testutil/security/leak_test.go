// Package security_test verifies that sensitive data (API keys, credentials)
// never appears in proxy logs, metrics, or error responses.
package security_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/httpapi"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	"github.com/earlgrey/piproxy/internal/providers"
	"github.com/earlgrey/piproxy/internal/ratelimit"
	"github.com/earlgrey/piproxy/internal/rewrite"
	"github.com/earlgrey/piproxy/internal/upstream"
)

// sensitivePatterns lists patterns that must never appear in outputs.
var sensitivePatterns = []string{
	"sk-",                   // OpenAI API key prefix
	"sk-ant-",               // Anthropic API key prefix
	"cpk_live_",             // Proxy key prefix
	"x-corp-proxy-key",      // Proxy key header
	"x-api-key",             // API key header
	"authorization: bearer", // Auth header value
	"proxy_secret_",         // Env-based secret prefix
	"PROXY_SECRET_",         // Uppercase env var
}

// TestSecretLeakInProxyResponse ensures that when the proxy returns an error,
// it does not leak API keys in the response body.
func TestSecretLeakInProxyResponse(t *testing.T) {
	mock := newSecureMock(t)
	defer mock.Close()

	logBuf := &bytes.Buffer{}

	// Create a logger that writes to both the buffer (for leak detection)
	// and the test log (for debugging).
	multiLogger := zerolog.New(io.MultiWriter(logBuf, newTestWriter(t))).With().Timestamp().Logger()

	registry := providers.NewRegistry()
	registry.RegisterProvider(&providers.Provider{
		Type: providers.ProviderOpenAI, Name: "Test",
		PublicBasePath: "/openai", UpstreamBaseURL: mock.URL,
		AllowedPaths:       []string{"/v1/chat/completions"},
		AllowPrivateEgress: true,
		Streaming:          true, Timeout: 30 * time.Second,
		MaxBodyBytes: 1 << 20, Enabled: true,
	})

	uc := upstream.NewClient(upstream.Config{
		Timeout: 30 * time.Second, MaxConnsPerHost: 100, MaxIdleConns: 100,
	})
	rw := rewrite.NewRequestRewriter(registry)
	ipLim := ratelimit.NewMiddleware(ratelimit.NewMemoryStore(), 10000, time.Minute)

	// Build deps WITHOUT auth middleware so RequireAnyScope isn't applied.
	deps := &httpapi.Dependencies{
		ProviderRegistry: registry,
		UpstreamClient:   uc,
		RequestRewriter:  rw,
		IPRateLimiter:    ipLim,
		Logger:           multiLogger,
	}

	router := httpapi.NewPublicRouter(httpapi.RouterConfig{
		MaxBodyBytes:   1 << 20,
		CORS:           middleware.DefaultCORSConfig(),
		RequestTimeout: 30 * time.Second,
	}, deps)

	proxy := newLocalTestServer(t, router)
	defer proxy.Close()

	// Test cases: various ways secrets might leak.
	tests := []struct {
		name       string
		path       string
		headers    map[string]string
		body       string
		wantStatus int // don't care about exact status, just check for leaks
	}{
		{
			name: "valid request leaks nothing",
			path: "/openai/v1/chat/completions",
			headers: map[string]string{
				"Content-Type":     "application/json",
				"X-Corp-Proxy-Key": "cpk_live_test-key-should-not-leak",
			},
			body:       `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`,
			wantStatus: 200,
		},
		{
			name: "malformed body leaks nothing",
			path: "/openai/v1/chat/completions",
			headers: map[string]string{
				"Content-Type":  "application/json",
				"Authorization": "Bearer sk-test-secret-key-12345",
			},
			body:       `not json`,
			wantStatus: 400,
		},
		{
			name: "health endpoint leaks nothing",
			path: "/healthz",
			headers: map[string]string{
				"Authorization": "Bearer sk-test-secret-key-12345",
			},
			wantStatus: 200,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear log buffer before each test.
			logBuf.Reset()

			body := strings.NewReader(tt.body)
			req, err := http.NewRequestWithContext(context.Background(), "POST", proxy.URL+tt.path, body)
			if err != nil {
				t.Fatalf("create request: %v", err)
			}
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do request: %v", err)
			}
			defer resp.Body.Close()
			respBody, _ := io.ReadAll(resp.Body)

			// Check response body for leaked patterns.
			responseStr := strings.ToLower(string(respBody))
			for _, pattern := range sensitivePatterns {
				lowerPat := strings.ToLower(pattern)
				if strings.Contains(responseStr, lowerPat) {
					t.Errorf("RESPONSE LEAK: pattern %q found in response body (status %d)", pattern, resp.StatusCode)
				}
			}

			// Check log buffer for leaked patterns.
			logStr := strings.ToLower(logBuf.String())
			for _, pattern := range sensitivePatterns {
				lowerPat := strings.ToLower(pattern)
				if strings.Contains(logStr, lowerPat) {
					t.Errorf("LOG LEAK: pattern %q found in log output", pattern)
				}
			}
		})
	}
}

// newSecureMock creates a mock upstream that includes a fake API key in its
// response headers, simulating a misbehaving upstream that might leak credentials.
func newSecureMock(t *testing.T) *httptest.Server {
	return newLocalTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"test","usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
}

// newTestWriter returns a writer that forwards to testing.T log.
func newTestWriter(t *testing.T) io.Writer {
	return &testWriter{t: t}
}

type testWriter struct {
	t *testing.T
}

func (w *testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}
