package middleware_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
)

func TestRecover_PanicWrites500(t *testing.T) {
	handler := middleware.RequestID(middleware.Recover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})))

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", nil)
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("expected JSON content type, got %q", ct)
	}
	if !strings.Contains(w.Body.String(), "internal_error") {
		t.Fatalf("expected internal_error body, got %q", w.Body.String())
	}
	if w.Header().Get("X-Request-ID") == "" {
		t.Fatal("expected X-Request-ID to still be set when RequestID wraps Recover")
	}
}

func TestRecover_LogsPanicsWithInnerRequestContext(t *testing.T) {
	var logs bytes.Buffer
	logger := zerolog.New(&logs).Level(zerolog.InfoLevel)
	handler := middleware.Recover(middleware.RequestID(middleware.RequestLogging(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("observable boom")
	}))))

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/proxy/v1/context", nil)
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
	for _, want := range []string{
		`"message":"request.panic"`,
		`"method":"POST"`,
		`"path":"/proxy/v1/context"`,
		`"request_id":"req_`,
		`"status":500`,
		`"error_class":"panic"`,
		`"panic":"observable boom"`,
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("panic log missing %s: %s", want, logs.String())
		}
	}
}

func TestRecover_NoPanicPassthrough(t *testing.T) {
	handler := middleware.Recover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("ok"))
	}))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", w.Code)
	}
	if w.Body.String() != "ok" {
		t.Fatalf("expected body unchanged, got %q", w.Body.String())
	}
}

func TestRecover_HeadersAlreadySent(t *testing.T) {
	handler := middleware.Recover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		panic("after write")
	}))

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	// Recover must not attempt a second WriteHeader; status stays 200.
	if w.Code != http.StatusOK {
		t.Fatalf("expected status to stay 200, got %d", w.Code)
	}
	if w.Body.String() != "partial" {
		t.Fatalf("expected partial body untouched, got %q", w.Body.String())
	}
}

func TestRecover_InvokesRollback(t *testing.T) {
	var calls int
	var rb func()
	handler := middleware.Recover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(middleware.WithRollback(r.Context(), func() { calls++ }))
		rb = middleware.RollbackFromContext(r.Context())
		panic("boom after reserve")
	}))

	// The rollback is registered on the handler's local request after Recover
	// dispatches, matching provider handlers after CheckQuota succeeds.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	handler.ServeHTTP(w, r)

	if calls != 1 {
		t.Fatalf("expected rollback invoked exactly once, got %d", calls)
	}

	// The holder is consume-once: invoking it again must be a no-op (defends
	// against the success path also rolling back the same reservation).
	if rb != nil {
		rb()
	}
	if calls != 1 {
		t.Fatalf("expected rollback to remain consume-once, got %d calls", calls)
	}
}

func TestRecover_RethrowsErrAbortHandler(t *testing.T) {
	handler := middleware.Recover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	defer func() {
		rec := recover()
		if rec != http.ErrAbortHandler {
			t.Fatalf("expected ErrAbortHandler to be re-panicked, got %v", rec)
		}
	}()

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	t.Fatal("expected handler to re-panic ErrAbortHandler")
}

func TestRecover_PreservesFlusher(t *testing.T) {
	var implementsFlusher bool
	handler := middleware.Recover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, implementsFlusher = w.(http.Flusher)
		w.WriteHeader(http.StatusOK)
	}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if !implementsFlusher {
		t.Fatal("expected wrapped writer to implement http.Flusher for SSE")
	}
}
