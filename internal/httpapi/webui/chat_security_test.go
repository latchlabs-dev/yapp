package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/auth"
	"github.com/earlgrey/piproxy/internal/duffel"
	"github.com/earlgrey/piproxy/internal/hooks"
	"github.com/earlgrey/piproxy/internal/memory"
	"github.com/earlgrey/piproxy/internal/providers"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

type promptMemoryReader struct{ reads atomic.Int64 }

func (m *promptMemoryReader) ReadFile(_ context.Context, path string) (string, error) {
	m.reads.Add(1)
	if strings.HasSuffix(path, "/memory.md") {
		return "# Project memory\n\nPRIVATE PROJECT NOTE: we store invoices in the restricted warehouse.\n", nil
	}
	return "", nil
}

func (*promptMemoryReader) Search(context.Context, string, duffel.SearchOpts) ([]duffel.SearchResult, error) {
	return nil, nil
}

func TestBuildSystemPromptRequiresMemoryReadScope(t *testing.T) {
	reader := &promptMemoryReader{}
	deps := &Dependencies{MemoryBuilder: memory.NewBuilder(reader, memory.BundleConfig{}, nil, zerolog.Nop())}
	conv := &pgstore.WebConversation{ProjectID: "restricted-project"}
	for _, scopes := range [][]string{nil, {auth.ScopeLLMInvoke}, {auth.ScopeLLMInvoke, auth.ScopeMemoryObserve}} {
		prompt, _, _ := deps.buildSystemPrompt(context.Background(), &auth.Context{TenantID: "tenant-a", Scopes: scopes}, conv, "")
		if prompt != webPreamble || reader.reads.Load() != 0 {
			t.Fatalf("scope %v loaded unauthorized memory: %q (%d reads)", scopes, prompt, reader.reads.Load())
		}
	}
	prompt, degraded, reason := deps.buildSystemPrompt(context.Background(), &auth.Context{TenantID: "tenant-a", Scopes: []string{auth.ScopeMemoryContextRead}}, conv, "")
	if degraded || reason != "" || !strings.Contains(prompt, "PRIVATE PROJECT NOTE") || reader.reads.Load() == 0 {
		t.Fatalf("authorized memory missing: %q, degraded %v, reason %q", prompt, degraded, reason)
	}
}

type reviewedEventSink struct{ events chan hooks.Event }

func (s *reviewedEventSink) Name() string { return "review-test" }
func (*reviewedEventSink) Close() error   { return nil }
func (s *reviewedEventSink) Write(_ context.Context, ev *hooks.Event) error {
	if ev.Type != hooks.EventUserMessagePostSend {
		return nil
	}
	// Copy the event so assertions inspect the observer's actual payload.
	encoded, _ := json.Marshal(ev)
	var copy hooks.Event
	_ = json.Unmarshal(encoded, &copy)
	s.events <- copy
	return nil
}

func TestReviewOutputEnforcesRefusalsAndFailModes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		decision  *hooks.Decision
		status    int
		failMode  string
		wantReply string
		wantError string
	}{
		{"abort without mutation", &hooks.Decision{Abort: true, Message: "blocked by policy"}, 200, hooks.FailOpen, "", "hook_blocked"},
		{"fail closed", nil, 503, hooks.FailClosed, "", "hook_unavailable"},
		{"fail open", nil, 503, hooks.FailOpen, "DRAFT SECRET", ""},
		{"rewrite", &hooks.Decision{RewrittenOutput: "APPROVED"}, 200, hooks.FailClosed, "APPROVED", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(tc.decision)
			}))
			defer server.Close()
			sink := &reviewedEventSink{events: make(chan hooks.Event, 2)}
			manager := hooks.NewManager(hooks.Config{Sinks: []hooks.Sink{sink}}, zerolog.Nop())
			manager.SetAllowPrivateHookEgress(true)
			manager.SetSubscriptions(hooks.SubscriptionFunc(func() []*hooks.Subscription {
				return []*hooks.Subscription{{ID: "review", URL: server.URL, EventType: hooks.EventUserMessagePostSend,
					TenantID: "tenant-a", Enabled: true, Mode: hooks.ModeBlocking, IncludeContent: true,
					AllowMutation: tc.name == "rewrite", FailMode: tc.failMode}}
			}))
			deps := &Dependencies{Hooks: manager, Logger: zerolog.Nop()}
			if !deps.willBufferForHooks("tenant-a", "project") {
				t.Fatal("blocking review without mutation did not require buffering")
			}
			reply, _, rejected := deps.reviewOutput(context.Background(), &auth.Context{TenantID: "tenant-a", UserID: "alice"},
				&pgstore.WebConversation{ID: "thread", ProjectID: "project"}, testRoute(providers.ProviderOpenAI, "/openai"), "system", nil, "DRAFT SECRET", tokenUsage{})
			if reply != tc.wantReply || (rejected != nil) != (tc.wantError != "") {
				t.Fatalf("review = %q, %+v; want %q, %q", reply, rejected, tc.wantReply, tc.wantError)
			}
			if rejected != nil && rejected.Type != tc.wantError {
				t.Fatalf("review error type = %q, want %q", rejected.Type, tc.wantError)
			}
			manager.Close()
			if len(sink.events) != 1 {
				t.Fatalf("observer received %d post events, want one", len(sink.events))
			}
			ev := <-sink.events
			if ev.Content == nil || ev.Content.Output != tc.wantReply {
				t.Fatalf("observer received draft instead of policy result: %+v", ev.Content)
			}
		})
	}
}

func TestAsyncOnlyPostHooksKeepStreaming(t *testing.T) {
	manager := hooks.NewManager(hooks.Config{}, zerolog.Nop())
	defer manager.Close()
	manager.SetSubscriptions(hooks.SubscriptionFunc(func() []*hooks.Subscription {
		return []*hooks.Subscription{{ID: "observer", EventType: hooks.EventUserMessagePostSend, Enabled: true, Mode: hooks.ModeAsync}}
	}))
	if (&Dependencies{Hooks: manager}).willBufferForHooks("tenant-a", "project") {
		t.Fatal("async-only observers unexpectedly disabled streaming")
	}
}

func TestInProcessPostReviewBlocksWithoutPublishingDraft(t *testing.T) {
	sink := &reviewedEventSink{events: make(chan hooks.Event, 2)}
	manager := hooks.NewManager(hooks.Config{Sinks: []hooks.Sink{sink}}, zerolog.Nop())
	var calls atomic.Int64
	manager.Append(hooks.HandlerFunc{HandlerName: "refusal", EventTypes: []hooks.EventType{hooks.EventUserMessagePostSend},
		Fn: func(context.Context, *hooks.Event) (*hooks.Decision, error) {
			calls.Add(1)
			return &hooks.Decision{Abort: true}, nil
		}})
	deps := &Dependencies{Hooks: manager, Logger: zerolog.Nop()}
	if !deps.willBufferForHooks("tenant-a", "project") {
		t.Fatal("in-process output decision did not require buffering")
	}
	reply, _, rejected := deps.reviewOutput(context.Background(), &auth.Context{TenantID: "tenant-a"}, &pgstore.WebConversation{},
		testRoute(providers.ProviderOpenAI, "/openai"), "", nil, "DRAFT", tokenUsage{})
	manager.Close()
	if reply != "" || rejected == nil || calls.Load() != 1 || len(sink.events) != 1 {
		t.Fatalf("review reply %q, rejection %+v, handler calls %d, observer calls %d", reply, rejected, calls.Load(), len(sink.events))
	}
	if ev := <-sink.events; ev.Content.Output != "" || ev.Result.Status != "blocked" {
		t.Fatalf("blocked output exposed: %+v", ev)
	}
}

func TestHeldTurnLocalDeadlineStopsOutput(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	h := &heldTurn{ctx: ctx, until: time.Now().Add(-time.Second)}
	r := newTurnRecorder(newSSEWriter(httptest.NewRecorder()), protocolOpenAI, false)
	r.canWrite = h.canWrite
	if n, err := r.Write([]byte("draft")); n != 0 || err == nil {
		t.Fatalf("expired local lease still wrote output: %d / %v", n, err)
	}
}
