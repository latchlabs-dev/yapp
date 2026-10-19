package anthropic_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/earlgrey/piproxy/internal/httpapi/anthropic"
	"github.com/earlgrey/piproxy/internal/httpapi/dataplane"
	"github.com/earlgrey/piproxy/internal/providers"
	"github.com/earlgrey/piproxy/internal/rewrite"
	"github.com/earlgrey/piproxy/internal/upstream"
)

func TestMessagesHandlerContextOverflowReturnsBadRequest(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local listener unavailable: %v", err)
	}
	upstreamServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"prompt is too long"}}`))
	}))
	upstreamServer.Listener = listener
	upstreamServer.Start()
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
		dataplane.Services{},
	)

	req := httptest.NewRequest("POST", "/anthropic/v1/messages", strings.NewReader(`{"model":"claude-test","messages":[],"max_tokens":1}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.Handle(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "context_length_exceeded") {
		t.Fatalf("expected context overflow response, got %s", rec.Body.String())
	}
}
