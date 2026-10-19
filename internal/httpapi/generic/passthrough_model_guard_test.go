package generic

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/earlgrey/piproxy/internal/httpapi/dataplane"
	"github.com/earlgrey/piproxy/internal/modelguard"
	"github.com/earlgrey/piproxy/internal/providers"
	"github.com/earlgrey/piproxy/internal/rewrite"
	"github.com/earlgrey/piproxy/internal/upstream"
)

func TestPassthroughHandlerModelGuardBlocksBeforeUpstream(t *testing.T) {
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream must not be called for a guarded model")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstreamServer.Close()

	registry := providers.NewRegistry()
	provider := &providers.Provider{
		Type:               providers.ProviderGoogleGenerativeAI,
		PublicBasePath:     "/gemini",
		UpstreamBaseURL:    upstreamServer.URL,
		AllowedPaths:       []string{"/v1beta/models/gemini-pro:generateContent"},
		AllowPrivateEgress: true,
		Enabled:            true,
	}
	if err := registry.RegisterProvider(provider); err != nil {
		t.Fatalf("RegisterProvider failed: %v", err)
	}

	handler := NewPassthroughHandler(
		upstream.NewClient(upstream.Config{Timeout: 2 * time.Second, MaxConnsPerHost: 10, MaxIdleConns: 10}),
		rewrite.NewRequestRewriter(registry),
		registry,
		dataplane.Services{ModelGuards: modelguard.Rules{
			{Model: "gemini-old-pro", Message: "gemini-old-pro is retired — use gemini-flash instead."},
		}},
	)

	req := httptest.NewRequest("POST", "/gemini/v1beta/models/gemini-old-pro:generateContent",
		strings.NewReader(`{"model":"gemini-old-pro","contents":[]}`))
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
	if !strings.Contains(body, "use gemini-flash instead") {
		t.Errorf("custom message missing from %s", body)
	}
}
