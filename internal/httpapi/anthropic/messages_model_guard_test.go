package anthropic_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/earlgrey/piproxy/internal/httpapi/anthropic"
	"github.com/earlgrey/piproxy/internal/httpapi/dataplane"
	"github.com/earlgrey/piproxy/internal/modelguard"
	"github.com/earlgrey/piproxy/internal/providers"
	"github.com/earlgrey/piproxy/internal/rewrite"
	"github.com/earlgrey/piproxy/internal/upstream"
)

func TestMessagesHandlerModelGuardBlocksBeforeUpstream(t *testing.T) {
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream must not be called for a guarded model")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstreamServer.Close()

	registry := providers.NewRegistry()
	provider := &providers.Provider{
		Type:               providers.ProviderAnthropic,
		PublicBasePath:     "/anthropic",
		UpstreamBaseURL:    upstreamServer.URL,
		AllowedPaths:       []string{"/v1/messages"},
		AllowPrivateEgress: true,
		Enabled:            true,
	}
	if err := registry.RegisterProvider(provider); err != nil {
		t.Fatalf("RegisterProvider failed: %v", err)
	}

	handler := anthropic.NewMessagesHandler(
		upstream.NewClient(upstream.Config{Timeout: 2 * time.Second, MaxConnsPerHost: 10, MaxIdleConns: 10}),
		rewrite.NewRequestRewriter(registry),
		registry,
		dataplane.Services{ModelGuards: modelguard.Rules{
			{Model: "claude-old-pro", Message: "claude-old-pro is retired — use claude-new-flash instead."},
		}},
	)

	req := httptest.NewRequest("POST", "/anthropic/v1/messages",
		strings.NewReader(`{"model":"claude-old-pro","messages":[],"max_tokens":8}`))
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
	if !strings.Contains(body, "use claude-new-flash instead") {
		t.Errorf("custom message missing from %s", body)
	}
}
