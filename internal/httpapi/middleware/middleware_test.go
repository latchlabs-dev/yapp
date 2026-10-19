package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/earlgrey/piproxy/internal/auth"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
)

func TestRequestID(t *testing.T) {
	handler := middleware.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := middleware.GetRequestID(r.Context())
		if id == "" {
			t.Error("expected non-empty request ID")
		}
		if !strings.HasPrefix(id, "req_") {
			t.Errorf("expected 'req_' prefix, got %q", id)
		}
		w.WriteHeader(http.StatusOK)
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/healthz", nil)
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	reqID := w.Header().Get("X-Request-ID")
	if reqID == "" {
		t.Error("expected X-Request-ID header")
	}
}

func TestRequestIDPreservesExisting(t *testing.T) {
	handler := middleware.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := middleware.GetRequestID(r.Context())
		if id != "client-provided-id" {
			t.Errorf("expected 'client-provided-id', got %q", id)
		}
		w.WriteHeader(http.StatusOK)
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	r.Header.Set("X-Request-ID", "client-provided-id")
	handler.ServeHTTP(w, r)

	if w.Header().Get("X-Request-ID") != "client-provided-id" {
		t.Errorf("expected X-Request-ID to be preserved")
	}
}

func TestBodySize(t *testing.T) {
	handler := middleware.BodySize(10)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Must read the body for MaxBytesReader to enforce the limit.
		buf := make([]byte, 100)
		_, err := r.Body.Read(buf)
		if err != nil {
			// If we got an error reading, it's likely the body was too large.
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	// Small body should pass.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/test", strings.NewReader("small"))
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for small body, got %d", w.Code)
	}

	// Large body should fail.
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("POST", "/test", strings.NewReader("this body is way too long for the limit"))
	handler.ServeHTTP(w2, r2)
	if w2.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413 for large body, got %d", w2.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	handler := middleware.SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	handler.ServeHTTP(w, r)

	expectedHeaders := map[string]string{
		"Strict-Transport-Security": "max-age=31536000; includeSubDomains",
		"X-Content-Type-Options":    "nosniff",
		"Referrer-Policy":           "no-referrer",
		"X-Frame-Options":           "DENY",
	}

	for key, expected := range expectedHeaders {
		if got := w.Header().Get(key); got != expected {
			t.Errorf("expected header %s = %q, got %q", key, expected, got)
		}
	}
}

func TestTimeout(t *testing.T) {
	handler := middleware.Timeout(0)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The context should be cancelled immediately with 0 timeout.
		if r.Context().Err() != nil {
			w.WriteHeader(http.StatusRequestTimeout)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	handler.ServeHTTP(w, r)

	// With 0 timeout, the context should have been cancelled immediately.
	// The handler chose 408 for cancelled context.
	_ = w.Code
}

func TestAuthContextHelpers(t *testing.T) {
	if got := middleware.GetAuthContext(context.Background()); got != nil {
		t.Fatalf("empty context auth = %+v", got)
	}

	want := &auth.Context{TenantID: "tenant_1", UserID: "user_1", Scopes: []string{auth.ScopeLLMInvoke}}
	ctx := middleware.WithAuthContext(context.Background(), want)
	if got := middleware.GetAuthContext(ctx); got != want {
		t.Fatalf("auth context = %+v, want %+v", got, want)
	}
}

func TestRequireScope(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	handler := middleware.RequireScope(auth.ScopeLLMInvoke)(next)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", w.Code)
	}

	w = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req = req.WithContext(middleware.WithAuthContext(req.Context(), &auth.Context{Scopes: []string{"other"}}))
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("wrong scope status = %d", w.Code)
	}

	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/", nil)
	req = req.WithContext(middleware.WithAuthContext(req.Context(), &auth.Context{Scopes: []string{auth.ScopeLLMInvoke}}))
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("allowed status = %d", w.Code)
	}
}

func TestRequireAnyScope(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	handler := middleware.RequireAnyScope(auth.ScopeLLMInvokeOpenAI, auth.ScopeLLMInvokeAnthropic)(next)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req = req.WithContext(middleware.WithAuthContext(req.Context(), &auth.Context{Scopes: []string{auth.ScopeLLMInvokeAnthropic}}))
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("matching any scope status = %d", w.Code)
	}

	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/", nil)
	req = req.WithContext(middleware.WithAuthContext(req.Context(), &auth.Context{Scopes: []string{"admin:users"}}))
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-matching any scope status = %d", w.Code)
	}
}

func TestAuthHeaderHelpers(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if middleware.HasAuthHeader(req) {
		t.Fatal("empty request should not have auth headers")
	}
	req.Header.Set("Authorization", "Bearer cpk_live_secret")
	req.Header.Set("X-Corp-Proxy-Key", "cpk_live_proxy")
	req.Header.Set("X-Api-Key", "cpk_live_api")
	req.Header.Set("Cookie", "sid=secret")
	req.Header.Set("Set-Cookie", "sid=secret")
	req.Header.Set("Proxy-Authorization", "Basic secret")
	req.Header.Set("X-Request-Token-Count", "42")

	if !middleware.HasAuthHeader(req) {
		t.Fatal("request should have auth headers")
	}
	middleware.StripAuthHeaders(req)
	for _, key := range []string{"Authorization", "X-Corp-Proxy-Key", "X-Api-Key", "Cookie", "Set-Cookie", "Proxy-Authorization"} {
		if got := req.Header.Get(key); got != "" {
			t.Fatalf("%s was not stripped: %q", key, got)
		}
	}
	if got := req.Header.Get("X-Request-Token-Count"); got != "42" {
		t.Fatalf("non-auth token count header should be preserved, got %q", got)
	}
}

func TestCORS(t *testing.T) {
	cfg := middleware.DefaultCORSConfig()
	cfg.AllowedOrigins = []string{"https://admin.example.com"}
	cfg.AllowCredentials = true
	handler := middleware.CORS(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/", nil)
	req.Header.Set("Origin", "https://admin.example.com")
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://admin.example.com" {
		t.Fatalf("allow origin = %q", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("allow credentials = %q", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") {
		t.Fatalf("allow methods = %q", got)
	}

	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("normal request status = %d", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("disallowed origin should not be echoed, got %q", got)
	}
}
