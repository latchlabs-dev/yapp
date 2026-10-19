package openai_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/earlgrey/piproxy/internal/httpapi/dataplane"
	"github.com/earlgrey/piproxy/internal/httpapi/openai"
	"github.com/earlgrey/piproxy/internal/modelguard"
	"github.com/earlgrey/piproxy/internal/providers"
	"github.com/earlgrey/piproxy/internal/rewrite"
	"github.com/earlgrey/piproxy/internal/upstream"
)

// guardTestUpstream fails the test if the request ever reaches upstream: the
// guard must fire before routing.
func guardTestUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		t.Errorf("upstream must not be called for a guarded model")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(func() {
		srv.Close()
		if calls.Load() > 0 {
			t.Fatalf("upstream was called %d times", calls.Load())
		}
	})
	return srv
}

func guardTestChatHandler(t *testing.T, upstreamServer *httptest.Server) *openai.ChatHandler {
	t.Helper()
	registry := providers.NewRegistry()
	provider := &providers.Provider{
		Type:               providers.ProviderOpenAI,
		PublicBasePath:     "/openai",
		UpstreamBaseURL:    upstreamServer.URL,
		AllowedPaths:       []string{"/v1/chat/completions"},
		AllowPrivateEgress: true,
		Enabled:            true,
	}
	if err := registry.RegisterProvider(provider); err != nil {
		t.Fatalf("RegisterProvider failed: %v", err)
	}
	return openai.NewChatHandler(
		upstream.NewClient(upstream.Config{Timeout: 2 * time.Second, MaxConnsPerHost: 10, MaxIdleConns: 10}),
		rewrite.NewRequestRewriter(registry),
		registry,
		dataplane.Services{ModelGuards: modelguard.Rules{
			{Model: "deepseek-v4-pro", Message: "deepseek-v4-pro is not available — deepseek-v4-flash is cheaper and better, use that instead."},
		}},
	)
}

func TestChatHandlerModelGuardBlocksBeforeUpstream(t *testing.T) {
	upstreamServer := guardTestUpstream(t)
	handler := guardTestChatHandler(t, upstreamServer)

	req := httptest.NewRequest("POST", "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4-pro","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.Handle(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "model_blocked") {
		t.Errorf("error type model_blocked missing from %s", body)
	}
	if !strings.Contains(body, "cheaper and better") {
		t.Errorf("custom message missing from %s", body)
	}
}

func TestChatHandlerModelGuardMatchesCorpPrefix(t *testing.T) {
	upstreamServer := guardTestUpstream(t)
	handler := guardTestChatHandler(t, upstreamServer)

	req := httptest.NewRequest("POST", "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"corp-deepseek-v4-pro","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.Handle(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model_blocked") {
		t.Errorf("error type model_blocked missing from %s", rec.Body.String())
	}
}

func TestChatHandlerModelGuardAllowsUnrelatedModel(t *testing.T) {
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstreamServer.Close()
	handler := guardTestChatHandler(t, upstreamServer)

	req := httptest.NewRequest("POST", "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4-flash","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.Handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unguarded model should pass, got %d: %s", rec.Code, rec.Body.String())
	}
}
