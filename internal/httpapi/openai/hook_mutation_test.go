package openai_test

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/hooks"
	"github.com/earlgrey/piproxy/internal/httpapi/dataplane"
	"github.com/earlgrey/piproxy/internal/httpapi/openai"
	"github.com/earlgrey/piproxy/internal/providers"
	"github.com/earlgrey/piproxy/internal/rewrite"
	"github.com/earlgrey/piproxy/internal/upstream"
)

// hookTestRig wires a stub upstream, a stub hook endpoint, and a chat handler
// with DB-managed hooks enabled, so a hook's effect can be observed on the
// bytes that actually reach the provider.
type hookTestRig struct {
	handler      *openai.ChatHandler
	upstreamBody func() []byte
}

func newHookTestRig(t *testing.T, decision *hooks.Decision, sub *hooks.Subscription) *hookTestRig {
	t.Helper()

	hookServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if decision == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_ = json.NewEncoder(w).Encode(decision)
	}))
	t.Cleanup(hookServer.Close)

	var seen []byte
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local listener unavailable: %v", err)
	}
	upstreamServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	upstreamServer.Listener = listener
	upstreamServer.Start()
	t.Cleanup(upstreamServer.Close)

	registry := providers.NewRegistry()
	if err := registry.RegisterProvider(&providers.Provider{
		Type:               providers.ProviderOpenAI,
		PublicBasePath:     "/openai",
		UpstreamBaseURL:    upstreamServer.URL,
		AllowedPaths:       []string{"/v1/chat/completions"},
		AllowPrivateEgress: true,
		Enabled:            true,
	}); err != nil {
		t.Fatalf("RegisterProvider: %v", err)
	}

	manager := hooks.NewManager(hooks.Config{Timeout: 2 * time.Second}, zerolog.Nop())
	t.Cleanup(manager.Close)
	sub.URL = hookServer.URL
	manager.SetSubscriptions(hooks.SubscriptionFunc(func() []*hooks.Subscription {
		return []*hooks.Subscription{sub}
	}))
	manager.SetAllowPrivateHookEgress(true)

	return &hookTestRig{
		handler: openai.NewChatHandler(
			upstream.NewClient(upstream.Config{Timeout: 2 * time.Second, MaxConnsPerHost: 10, MaxIdleConns: 10}),
			rewrite.NewRequestRewriter(registry),
			registry,
			dataplane.Services{Hooks: manager},
		),
		upstreamBody: func() []byte { return seen },
	}
}

func mutatingSub() *hooks.Subscription {
	return &hooks.Subscription{
		ID: "s1", Name: "guardrail", EventType: hooks.EventUserMessagePreSend,
		Mode: hooks.ModeBlocking, FailMode: hooks.FailOpen,
		IncludeContent: true, AllowMutation: true, Enabled: true, Timeout: time.Second,
	}
}

// The point of a pre-send rewrite is that the provider receives the modified
// request, not that the decision is merely reported. This asserts on the bytes
// the upstream actually saw.
func TestPreSendHookSystemPrependReachesUpstream(t *testing.T) {
	rig := newHookTestRig(t, &hooks.Decision{SystemPrepend: "COMPANY POLICY"}, mutatingSub())

	req := httptest.NewRequest("POST", "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	rig.handler.Handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := string(rig.upstreamBody())
	if !strings.Contains(body, "COMPANY POLICY") {
		t.Fatalf("upstream body did not carry the hook's system prepend: %s", body)
	}
}

func TestPreSendHookRedactionReachesUpstream(t *testing.T) {
	rig := newHookTestRig(t, &hooks.Decision{
		RewrittenMessages: []hooks.ContentMessage{{Role: "user", Text: "my card is [REDACTED]"}},
	}, mutatingSub())

	req := httptest.NewRequest("POST", "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-test","messages":[{"role":"user","content":"my card is 4111111111111111"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	rig.handler.Handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := string(rig.upstreamBody())
	if strings.Contains(body, "4111111111111111") {
		t.Fatal("the unredacted value reached the provider")
	}
	if !strings.Contains(body, "[REDACTED]") {
		t.Fatalf("the redacted value is missing from the upstream body: %s", body)
	}
}

// A hook without the mutation grant must not be able to alter the request by
// returning the field anyway.
func TestPreSendHookWithoutMutationGrantLeavesRequestUntouched(t *testing.T) {
	sub := mutatingSub()
	sub.AllowMutation = false
	rig := newHookTestRig(t, &hooks.Decision{SystemPrepend: "INJECTED"}, sub)

	req := httptest.NewRequest("POST", "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	rig.handler.Handle(rec, req)

	if strings.Contains(string(rig.upstreamBody()), "INJECTED") {
		t.Fatal("a hook without the mutation grant altered the request")
	}
}

func TestPreSendHookAbortBlocksTheRequest(t *testing.T) {
	sub := mutatingSub()
	rig := newHookTestRig(t, &hooks.Decision{
		Abort:     true,
		Message:   "blocked by data-loss policy",
		ErrorType: "dlp_blocked",
	}, sub)

	req := httptest.NewRequest("POST", "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-test","messages":[{"role":"user","content":"secrets"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	rig.handler.Handle(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "blocked by data-loss policy") {
		t.Errorf("response did not carry the hook's message: %s", rec.Body.String())
	}
	if len(rig.upstreamBody()) != 0 {
		t.Error("the provider was called despite the hook aborting the turn")
	}
}

// A hook that returns annotations without mutating still tells the caller a
// guardrail acted, which is what the console renders as a badge.
func TestPreSendHookAnnotationsAreSurfaced(t *testing.T) {
	rig := newHookTestRig(t, &hooks.Decision{
		Annotations: map[string]string{"pii": "redacted"},
	}, mutatingSub())

	req := httptest.NewRequest("POST", "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	rig.handler.Handle(rec, req)

	header := rec.Header().Get(dataplane.HookAnnotationsHeader)
	if !strings.Contains(header, "redacted") {
		t.Fatalf("%s = %q, want the hook's annotations", dataplane.HookAnnotationsHeader, header)
	}
}
