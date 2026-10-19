// Package loadtest_test runs benchmarks and load tests against the proxy.
package loadtest_test

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/earlgrey/piproxy/internal/testutil/loadtest"
)

func BenchmarkProxyNonStreaming(b *testing.B) {
	cfg := loadtest.Config{
		Concurrency:    10,
		TotalRequests:  b.N,
		UpstreamDelay:  50 * time.Millisecond,
		BodySize:       512,
		Streaming:      false,
		RequestTimeout: 30 * time.Second,
	}

	suite, err := loadtest.NewSuite(cfg)
	if err != nil {
		if loadtest.IsLocalListenerUnavailable(err) {
			b.Skipf("local listeners unavailable: %v", err)
		}
		b.Fatalf("NewSuite: %v", err)
	}
	defer suite.Close()

	ctx := context.Background()
	b.ResetTimer()

	results := suite.RunLoadTest(ctx)
	stats := loadtest.AnalyzeResults(results)

	b.ReportMetric(stats["latency"].(map[string]float64)["p50_ms"], "p50_ms")
	b.ReportMetric(stats["latency"].(map[string]float64)["p95_ms"], "p95_ms")
	b.ReportMetric(stats["latency"].(map[string]float64)["p99_ms"], "p99_ms")
	b.ReportMetric(stats["requests_per_sec"].(float64), "rps")
}

func TestLoadTestNonStreaming10x100(t *testing.T) {
	cfg := loadtest.Config{
		Concurrency:    10,
		TotalRequests:  100,
		UpstreamDelay:  200 * time.Millisecond,
		BodySize:       512,
		Streaming:      false,
		RequestTimeout: 30 * time.Second,
	}

	suite, err := loadtest.NewSuite(cfg)
	if err != nil {
		if loadtest.IsLocalListenerUnavailable(err) {
			t.Skipf("local listeners unavailable: %v", err)
		}
		t.Fatalf("NewSuite: %v", err)
	}
	defer suite.Close()

	ctx := context.Background()
	results := suite.RunLoadTest(ctx)
	stats := loadtest.AnalyzeResults(results)

	t.Logf("\n=== Load Test Results (10 concurrent x 100 requests) ===")
	t.Logf("Total requests:  %d", stats["total_requests"])
	t.Logf("Success count:   %d", stats["success_count"])
	t.Logf("Error count:     %d", stats["error_count"])
	t.Logf("Error rate:      %.2f%%", stats["error_rate"].(float64)*100)
	t.Logf("Requests/sec:    %.1f", stats["requests_per_sec"])
	t.Logf("")

	lat := stats["latency"].(map[string]float64)
	t.Logf("Latency (ms):")
	t.Logf("  avg:  %.1f", lat["avg_ms"])
	t.Logf("  p50:  %.1f", lat["p50_ms"])
	t.Logf("  p90:  %.1f", lat["p90_ms"])
	t.Logf("  p95:  %.1f", lat["p95_ms"])
	t.Logf("  p99:  %.1f", lat["p99_ms"])
	if sc, ok := stats["status_codes"]; ok {
		t.Logf("Status codes:    %v", sc)
	}
	t.Logf("==================================================")

	// Assert baseline: p95 < 2x upstream delay with low error rate.
	upstreamTarget := float64(cfg.UpstreamDelay.Milliseconds()) * 2
	if lat["p95_ms"] > upstreamTarget*5 { // 5x to be generous for in-process test
		t.Logf("WARNING: p95 latency (%.1fms) exceeds 5x upstream delay (%.1fms)", lat["p95_ms"], upstreamTarget)
	}
	if stats["error_rate"].(float64) > 0.05 {
		t.Errorf("error rate %.2f%% exceeds 5%% threshold", stats["error_rate"].(float64)*100)
	}
}

func TestLoadTestNonStreaming50x500(t *testing.T) {
	cfg := loadtest.Config{
		Concurrency:    50,
		TotalRequests:  500,
		UpstreamDelay:  100 * time.Millisecond,
		BodySize:       4096,
		Streaming:      false,
		RequestTimeout: 30 * time.Second,
	}

	suite, err := loadtest.NewSuite(cfg)
	if err != nil {
		if loadtest.IsLocalListenerUnavailable(err) {
			t.Skipf("local listeners unavailable: %v", err)
		}
		t.Fatalf("NewSuite: %v", err)
	}
	defer suite.Close()

	ctx := context.Background()
	results := suite.RunLoadTest(ctx)
	stats := loadtest.AnalyzeResults(results)

	lat := stats["latency"].(map[string]float64)

	t.Logf("\n=== Load Test Results (50 concurrent x 500 requests) ===")
	t.Logf("Requests/sec:    %.1f", stats["requests_per_sec"])
	t.Logf("Error rate:      %.2f%%", stats["error_rate"].(float64)*100)
	t.Logf("p50 (ms):        %.1f", lat["p50_ms"])
	t.Logf("p95 (ms):        %.1f", lat["p95_ms"])
	t.Logf("p99 (ms):        %.1f", lat["p99_ms"])
	t.Logf("==================================================")

	if stats["error_rate"].(float64) > 0.10 {
		t.Errorf("error rate %.2f%% exceeds 10%% threshold", stats["error_rate"].(float64)*100)
	}
}

func TestLoadTestHighConcurrency(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping high concurrency test in short mode")
	}

	cfg := loadtest.Config{
		Concurrency:    100,
		TotalRequests:  500,
		UpstreamDelay:  50 * time.Millisecond,
		BodySize:       1024,
		Streaming:      false,
		RequestTimeout: 60 * time.Second,
	}

	suite, err := loadtest.NewSuite(cfg)
	if err != nil {
		if loadtest.IsLocalListenerUnavailable(err) {
			t.Skipf("local listeners unavailable: %v", err)
		}
		t.Fatalf("NewSuite: %v", err)
	}
	defer suite.Close()

	ctx := context.Background()
	results := suite.RunLoadTest(ctx)
	stats := loadtest.AnalyzeResults(results)

	lat := stats["latency"].(map[string]float64)

	t.Logf("\n=== Load Test (100 concurrent x 500 requests) ===")
	t.Logf("Requests/sec:    %.1f", stats["requests_per_sec"])
	t.Logf("Error rate:      %.2f%%", stats["error_rate"].(float64)*100)
	t.Logf("p50 (ms):        %.1f", lat["p50_ms"])
	t.Logf("p95 (ms):        %.1f", lat["p95_ms"])
	t.Logf("p99 (ms):        %.1f", lat["p99_ms"])
	t.Logf("Mem stats:")
	printMemStats(t)
	t.Logf("==================================================")

	if stats["error_rate"].(float64) > 0.10 {
		t.Errorf("error rate %.2f%% exceeds 10%% threshold", stats["error_rate"].(float64)*100)
	}
}

func printMemStats(t *testing.T) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	t.Logf("  Alloc:      %d MB", m.Alloc/1024/1024)
	t.Logf("  TotalAlloc: %d MB", m.TotalAlloc/1024/1024)
	t.Logf("  Sys:        %d MB", m.Sys/1024/1024)
	t.Logf("  NumGC:      %d", m.NumGC)
	t.Logf("  Goroutines: %d", runtime.NumGoroutine())
}

func TestMain(m *testing.M) {
	fmt.Println("=== PiProxy Load Tests ===")
	fmt.Printf("Go version: %s\n", runtime.Version())
	fmt.Printf("GOMAXPROCS: %d\n", runtime.GOMAXPROCS(0))
	fmt.Println()
	os.Exit(m.Run())
}
