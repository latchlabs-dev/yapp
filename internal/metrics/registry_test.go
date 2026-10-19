package metrics_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/earlgrey/piproxy/internal/metrics"
)

func TestNewRegistry(t *testing.T) {
	reg := metrics.NewRegistry()
	if reg == nil {
		t.Fatal("expected non-nil registry")
	}
}

func TestRegisterDefault(t *testing.T) {
	reg := metrics.NewRegistry()
	reg.RegisterDefault()
	// Should not panic.
}

func TestBuildInfoMetric(t *testing.T) {
	reg := metrics.NewRegistry()
	reg.BuildInfoMetric("1.0.0", "abc123", "go1.24")
	// Should not panic.
}

func TestMetricsHandler(t *testing.T) {
	reg := metrics.NewRegistry()
	reg.BuildInfoMetric("dev", "test", "go1.24")

	handler := reg.Handler()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/metrics", nil)
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "corp_proxy_build_info") {
		t.Error("expected build_info metric in output")
	}
	if !strings.Contains(body, "# HELP") {
		t.Error("expected HELP lines in Prometheus output")
	}
	if !strings.Contains(body, "# TYPE") {
		t.Error("expected TYPE lines in Prometheus output")
	}
}

func TestMetricsContentType(t *testing.T) {
	reg := metrics.NewRegistry()
	handler := reg.Handler()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/metrics", nil)
	handler.ServeHTTP(w, r)

	ct := w.Header().Get("Content-Type")
	if ct == "" {
		t.Error("expected Content-Type header")
	}
}

func TestMustRegister(t *testing.T) {
	reg := metrics.NewRegistry()
	// Should not panic on first register.
	reg.MustRegister()
	// MustRegister with nil should not panic.
}

func TestGoCollectorMetrics(t *testing.T) {
	reg := metrics.NewRegistry()
	reg.RegisterDefault()

	handler := reg.Handler()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/metrics", nil)
	handler.ServeHTTP(w, r)

	body := w.Body.String()
	if !strings.Contains(body, "go_goroutines") {
		t.Error("expected go_goroutines metric in output")
	}
	if !strings.Contains(body, "go_memstats_alloc_bytes") {
		t.Error("expected go_memstats_alloc_bytes metric in output")
	}
}

func TestAuthMetricsOutput(t *testing.T) {
	reg := metrics.NewRegistry()
	authMetrics := metrics.NewAuthMetrics(reg)
	authMetrics.ObserveAuth("ok")
	authMetrics.ObserveAuth("revoked_cache")

	body := scrapeMetrics(t, reg)
	if !strings.Contains(body, `corp_proxy_auth_requests_total{result="ok"} 1`) {
		t.Fatal("expected auth ok metric")
	}
	if !strings.Contains(body, `corp_proxy_auth_requests_total{result="revoked_cache"} 1`) {
		t.Fatal("expected auth revoked_cache metric")
	}

	var nilMetrics *metrics.AuthMetrics
	nilMetrics.ObserveAuth("ok")
}

func TestQuotaMetricsOutput(t *testing.T) {
	reg := metrics.NewRegistry()
	quotaMetrics := metrics.NewQuotaMetrics(reg)
	quotaMetrics.ObserveQuota("tenant", "allow")
	quotaMetrics.ObserveQuota("tenant", "denied:rpm")

	body := scrapeMetrics(t, reg)
	if !strings.Contains(body, `corp_proxy_quota_requests_total{decision="allow",scope_type="tenant"} 1`) {
		t.Fatal("expected quota allow metric")
	}
	if !strings.Contains(body, `corp_proxy_quota_requests_total{decision="denied:rpm",scope_type="tenant"} 1`) {
		t.Fatal("expected quota denied metric")
	}

	var nilMetrics *metrics.QuotaMetrics
	nilMetrics.ObserveQuota("tenant", "allow")
}

func TestUsageMetricsOutput(t *testing.T) {
	reg := metrics.NewRegistry()
	usageMetrics := metrics.NewUsageMetrics(reg)
	usageMetrics.ObserveTokens("gpt-4o", "input", 10)
	usageMetrics.ObserveTokens("gpt-4o", "output", 20)
	usageMetrics.ObserveCost("gpt-4o", 0.123)
	usageMetrics.ObserveRequest("gpt-4o", "openai-completions")

	body := scrapeMetrics(t, reg)
	if !strings.Contains(body, `corp_proxy_usage_tokens_total{direction="input",model="gpt-4o"} 10`) {
		t.Fatal("expected usage input token metric")
	}
	if !strings.Contains(body, `corp_proxy_usage_tokens_total{direction="output",model="gpt-4o"} 20`) {
		t.Fatal("expected usage output token metric")
	}
	if !strings.Contains(body, `corp_proxy_usage_cost_total{model="gpt-4o"} 0.123`) {
		t.Fatal("expected usage cost metric")
	}
	if !strings.Contains(body, `corp_proxy_usage_requests_total{model="gpt-4o",provider="openai-completions"} 1`) {
		t.Fatal("expected usage request metric")
	}

	var nilMetrics *metrics.UsageMetrics
	nilMetrics.ObserveTokens("gpt-4o", "input", 1)
	nilMetrics.ObserveCost("gpt-4o", 1)
	nilMetrics.ObserveRequest("gpt-4o", "openai-completions")
}

func scrapeMetrics(t *testing.T, reg *metrics.Registry) string {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/metrics", nil)
	reg.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	return w.Body.String()
}
