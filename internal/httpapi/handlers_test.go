package httpapi_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/earlgrey/piproxy/internal/config"
	"github.com/earlgrey/piproxy/internal/httpapi"
)

func TestHealthzHandler(t *testing.T) {
	router := httpapi.NewTestRouter()

	// GET /healthz should return 200.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/healthz", nil)
	router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("expected status 'ok', got %q", resp["status"])
	}
}

func TestReadyzHandler(t *testing.T) {
	router := httpapi.NewTestRouter()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/readyz", nil)
	router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestReadyzHandlerEscapesDependencyErrors(t *testing.T) {
	handler := httpapi.NewReadyzHandler(func() error {
		return fmt.Errorf(`postgres: bad "dsn"`)
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/readyz", nil)
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["status"] != "not_ready" {
		t.Fatalf("expected not_ready status, got %q", resp["status"])
	}
	if resp["reason"] != "dependency check failed" {
		t.Fatalf("unexpected reason: %q", resp["reason"])
	}
}

func TestVersionEndpointNotRegisteredOnPublicRouter(t *testing.T) {
	router := httpapi.NewTestRouter()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/version", nil)
	router.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestAdminDataRoutesNeverMountedOnPublicRouter(t *testing.T) {
	router := httpapi.NewPublicRouter(httpapi.RouterConfig{}, &httpapi.Dependencies{})
	paths := []string{
		"/conversation-payloads",
		"/session-submissions",
		"/projects",
		"/memory/projects",
		"/memory/search",
		"/tenants",
		"/usage-reports",
		"/insight-processors",
		"/insight-configurations",
		"/insight-tags",
		"/insight-runs",
		"/api/conversation-payloads",
		"/api/session-submissions",
		"/api/projects",
		"/api/memory/projects",
		"/api/insight-configurations",
		"/proxy/v1/context/../../conversation-payloads",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Authorization", "Bearer "+"cpk_"+"live_valid-user-shaped-key")
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("public path %s returned %d, want 404", path, rec.Code)
			}
		})
	}
}

func TestQuotaRouteFailsClosedWithoutAuthBackend(t *testing.T) {
	router := httpapi.NewPublicRouter(httpapi.RouterConfig{}, &httpapi.Dependencies{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/proxy/v1/quota", nil)

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var response map[string]map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response["error"]["type"] != "auth_unavailable" {
		t.Fatalf("unexpected error response: %#v", response)
	}
}

func TestSecurityHeadersOnRouter(t *testing.T) {
	router := httpapi.NewTestRouter()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/healthz", nil)
	router.ServeHTTP(w, r)

	if w.Header().Get("Strict-Transport-Security") == "" {
		t.Error("expected HSTS header")
	}
	if w.Header().Get("X-Content-Type-Options") == "" {
		t.Error("expected X-Content-Type-Options header")
	}
}

func TestNotImplementedEndpoints(t *testing.T) {
	router := httpapi.NewTestRouter()

	endpoints := []struct {
		method string
		path   string
	}{
		{"POST", "/openai/v1/chat/completions"},
		{"GET", "/openai/v1/models"},
		{"POST", "/deepseek/v1/chat/completions"},
		{"GET", "/deepseek/v1/models"},
		{"POST", "/anthropic/v1/messages"},
		{"GET", "/anthropic/v1/models"},
	}

	for _, ep := range endpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(ep.method, ep.path, nil)
			router.ServeHTTP(w, r)

			if w.Code != http.StatusNotImplemented {
				t.Errorf("expected 501, got %d", w.Code)
			}

			var resp map[string]interface{}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("failed to parse response: %v", err)
			}
		})
	}
}

func TestBuildInfoContents(t *testing.T) {
	info := config.BuildInfo()
	if info["version"] == "" {
		t.Error("expected version to be set")
	}
	if info["go_version"] == "" {
		t.Error("expected go_version to be set")
	}
}

func TestRouterHasRequestID(t *testing.T) {
	router := httpapi.NewTestRouter()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/healthz", nil)
	router.ServeHTTP(w, r)

	if w.Header().Get("X-Request-ID") == "" {
		t.Error("expected X-Request-ID header on all responses")
	}
}
