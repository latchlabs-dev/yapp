package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// hookServer is a stub endpoint that records what it was sent and answers with
// a scripted decision.
type hookServer struct {
	server *httptest.Server

	mu       sync.Mutex
	received []*Event
	headers  []http.Header

	decision *Decision
	status   int
	delay    time.Duration
}

func newHookServer(t *testing.T, decision *Decision) *hookServer {
	t.Helper()
	h := &hookServer{decision: decision, status: http.StatusOK}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ev := &Event{}
		_ = json.Unmarshal(body, ev)

		h.mu.Lock()
		h.received = append(h.received, ev)
		h.headers = append(h.headers, r.Header.Clone())
		delay, status, decision := h.delay, h.status, h.decision
		h.mu.Unlock()

		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if decision != nil {
			_ = json.NewEncoder(w).Encode(decision)
		}
	}))
	t.Cleanup(h.server.Close)
	return h
}

func (h *hookServer) calls() []*Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*Event(nil), h.received...)
}

func newTestManager(t *testing.T, subs ...*Subscription) *Manager {
	t.Helper()
	m := NewManager(Config{Timeout: 2 * time.Second}, zerolog.Nop())
	t.Cleanup(m.Close)
	m.SetSubscriptions(SubscriptionFunc(func() []*Subscription { return subs }))
	// Hook endpoints in these tests are on 127.0.0.1, which the SSRF guard
	// blocks by design.
	m.SetAllowPrivateHookEgress(true)
	return m
}

func testEvent() *Event {
	return &Event{
		Type:     EventUserMessagePreSend,
		TenantID: "tnt_1",
		Content: &EventContent{
			System:   "be brief",
			Messages: []ContentMessage{{Role: "user", Text: "hello"}},
		},
	}
}

func blockingSub(id, url string) *Subscription {
	return &Subscription{
		ID: id, Name: id, EventType: EventUserMessagePreSend, URL: url,
		Mode: ModeBlocking, FailMode: FailOpen, Enabled: true, Timeout: time.Second,
	}
}

// A subscription without include_content must never receive the prompt. This is
// the difference between a metrics webhook and a content-egress channel.
func TestSubscriptionWithoutIncludeContentSeesNoContent(t *testing.T) {
	server := newHookServer(t, nil)
	sub := blockingSub("s1", server.server.URL)
	sub.IncludeContent = false

	if _, err := newTestManager(t, sub).EmitBlocking(context.Background(), testEvent()); err != nil {
		t.Fatalf("emit: %v", err)
	}
	calls := server.calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if calls[0].Content != nil {
		t.Fatalf("content was delivered to a subscription that did not ask for it: %+v", calls[0].Content)
	}
	if calls[0].TenantID != "tnt_1" {
		t.Errorf("metadata was not delivered: %+v", calls[0])
	}
}

func TestSubscriptionWithIncludeContentSeesContent(t *testing.T) {
	server := newHookServer(t, nil)
	sub := blockingSub("s1", server.server.URL)
	sub.IncludeContent = true

	if _, err := newTestManager(t, sub).EmitBlocking(context.Background(), testEvent()); err != nil {
		t.Fatalf("emit: %v", err)
	}
	calls := server.calls()
	if len(calls) != 1 || calls[0].Content == nil {
		t.Fatalf("expected content to be delivered, got %+v", calls)
	}
	if calls[0].Content.Messages[0].Text != "hello" {
		t.Errorf("content = %+v", calls[0].Content)
	}
}

// A hook that was not granted allow_mutation cannot start rewriting prompts by
// simply returning the field.
func TestMutationIsIgnoredWithoutTheGrant(t *testing.T) {
	server := newHookServer(t, &Decision{SystemPrepend: "INJECTED", Annotations: map[string]string{"seen": "yes"}})
	sub := blockingSub("s1", server.server.URL)
	sub.IncludeContent = true
	sub.AllowMutation = false

	decision, err := newTestManager(t, sub).EmitBlocking(context.Background(), testEvent())
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if decision.SystemPrepend != "" {
		t.Errorf("SystemPrepend = %q, want it dropped without the mutation grant", decision.SystemPrepend)
	}
	if decision.Annotations["seen"] != "yes" {
		t.Error("annotations should still be honoured: they inform, they do not alter content")
	}
}

func TestMutationIsHonouredWithTheGrant(t *testing.T) {
	server := newHookServer(t, &Decision{SystemPrepend: "POLICY"})
	sub := blockingSub("s1", server.server.URL)
	sub.IncludeContent = true
	sub.AllowMutation = true

	decision, err := newTestManager(t, sub).EmitBlocking(context.Background(), testEvent())
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if decision.SystemPrepend != "POLICY" {
		t.Errorf("SystemPrepend = %q, want POLICY", decision.SystemPrepend)
	}
}

func TestAbortStopsLaterHooks(t *testing.T) {
	first := newHookServer(t, &Decision{Abort: true, Message: "nope"})
	second := newHookServer(t, nil)

	subA := blockingSub("a", first.server.URL)
	subA.Priority = 1
	subB := blockingSub("b", second.server.URL)
	subB.Priority = 2

	decision, err := newTestManager(t, subA, subB).EmitBlocking(context.Background(), testEvent())
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if decision == nil || !decision.Abort || decision.Message != "nope" {
		t.Fatalf("decision = %+v, want an abort carrying the hook's message", decision)
	}
	if len(second.calls()) != 0 {
		t.Error("a later hook ran after an abort")
	}
}

// A broken notification endpoint must not take model access down for a tenant.
func TestFailOpenLetsTheTurnProceed(t *testing.T) {
	server := newHookServer(t, nil)
	server.status = http.StatusInternalServerError
	sub := blockingSub("s1", server.server.URL)
	sub.FailMode = FailOpen

	if _, err := newTestManager(t, sub).EmitBlocking(context.Background(), testEvent()); err != nil {
		t.Fatalf("a fail-open hook returning 500 must not fail the turn, got %v", err)
	}
}

// A guardrail whose whole purpose is to stop something leaving must block the
// turn when it cannot be consulted.
func TestFailClosedRefusesTheTurn(t *testing.T) {
	server := newHookServer(t, nil)
	server.status = http.StatusInternalServerError
	sub := blockingSub("s1", server.server.URL)
	sub.FailMode = FailClosed

	_, err := newTestManager(t, sub).EmitBlocking(context.Background(), testEvent())
	if !errors.Is(err, ErrHookRefused) {
		t.Fatalf("err = %v, want ErrHookRefused", err)
	}
}

// The budget is shared across every hook, so several slow hooks cannot each
// spend the full allowance.
func TestBlockingBudgetIsSharedAcrossHooks(t *testing.T) {
	slow := newHookServer(t, nil)
	slow.delay = 400 * time.Millisecond
	later := newHookServer(t, nil)

	subA := blockingSub("a", slow.server.URL)
	subA.Priority = 1
	subB := blockingSub("b", later.server.URL)
	subB.Priority = 2

	manager := newTestManager(t, subA, subB)
	manager.SetBlockingBudget(150 * time.Millisecond)

	started := time.Now()
	if _, err := manager.EmitBlocking(context.Background(), testEvent()); err != nil {
		t.Fatalf("emit: %v", err)
	}
	elapsed := time.Since(started)
	if elapsed > time.Second {
		t.Fatalf("emit took %v; the budget did not bound the total hook time", elapsed)
	}
	if len(later.calls()) != 0 {
		t.Error("the second hook ran after the shared budget was exhausted")
	}
}

// Exhausting the budget is a failure to consult, so a fail-closed hook that
// never got its turn still refuses.
func TestBudgetExhaustionRespectsFailClosed(t *testing.T) {
	slow := newHookServer(t, nil)
	slow.delay = 400 * time.Millisecond
	later := newHookServer(t, nil)

	subA := blockingSub("a", slow.server.URL)
	subA.Priority = 1
	subB := blockingSub("b", later.server.URL)
	subB.Priority = 2
	subB.FailMode = FailClosed

	manager := newTestManager(t, subA, subB)
	manager.SetBlockingBudget(150 * time.Millisecond)

	if _, err := manager.EmitBlocking(context.Background(), testEvent()); !errors.Is(err, ErrHookRefused) {
		t.Fatalf("err = %v, want ErrHookRefused", err)
	}
}

// A narrowly scoped hook must see the turn before a broad observer does, so a
// project guardrail can redact before a tenant-wide logger records anything.
func TestProjectScopedHooksRunFirst(t *testing.T) {
	var order []string
	var mu sync.Mutex
	record := func(name string) *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}))
		t.Cleanup(s.Close)
		return s
	}
	global := record("global")
	tenant := record("tenant")
	project := record("project")

	subGlobal := blockingSub("g", global.URL)
	subTenant := blockingSub("t", tenant.URL)
	subTenant.TenantID = "tnt_1"
	subProject := blockingSub("p", project.URL)
	subProject.TenantID = "tnt_1"
	subProject.ProjectID = "mprj_1"

	ev := testEvent()
	ev.ProjectID = "mprj_1"
	if _, err := newTestManager(t, subGlobal, subTenant, subProject).EmitBlocking(context.Background(), ev); err != nil {
		t.Fatalf("emit: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"project", "tenant", "global"}
	if len(order) != 3 {
		t.Fatalf("order = %v, want all three hooks to run", order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestSubscriptionsDoNotMatchOtherTenants(t *testing.T) {
	server := newHookServer(t, nil)
	sub := blockingSub("s1", server.server.URL)
	sub.TenantID = "tnt_other"

	if _, err := newTestManager(t, sub).EmitBlocking(context.Background(), testEvent()); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if len(server.calls()) != 0 {
		t.Error("a hook scoped to another tenant received this tenant's event")
	}
}

func TestDeliveriesAreSigned(t *testing.T) {
	server := newHookServer(t, nil)
	sub := blockingSub("s1", server.server.URL)
	sub.Secret = "shhh"

	if _, err := newTestManager(t, sub).EmitBlocking(context.Background(), testEvent()); err != nil {
		t.Fatalf("emit: %v", err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.headers) != 1 {
		t.Fatal("no delivery recorded")
	}
	if server.headers[0].Get("X-Piproxy-Signature") == "" {
		t.Error("expected a signature header")
	}
	if server.headers[0].Get("X-Piproxy-Timestamp") == "" {
		t.Error("expected a timestamp header; without it a delivery is replayable")
	}
}

// A caller that streams needs to know before it starts whether the reply will
// be rewritten, because it must buffer instead.
func TestWillMutateOutput(t *testing.T) {
	mutating := &Subscription{
		ID: "m", EventType: EventUserMessagePostSend, URL: "https://example.com",
		Mode: ModeBlocking, AllowMutation: true, Enabled: true,
	}
	observing := &Subscription{
		ID: "o", EventType: EventUserMessagePostSend, URL: "https://example.com",
		Mode: ModeAsync, Enabled: true,
	}
	post := &Event{Type: EventUserMessagePostSend, TenantID: "tnt_1"}

	if newTestManager(t, observing).WillMutateOutput(post) {
		t.Error("a non-mutating hook must not force buffering")
	}
	if !newTestManager(t, mutating, observing).WillMutateOutput(post) {
		t.Error("a mutating post-send hook must force buffering")
	}
}

func TestOutputReviewRetainsEarlierHandlerRewrite(t *testing.T) {
	server := newHookServer(t, &Decision{Annotations: map[string]string{"subscription": "checked"}})
	sub := blockingSub("review", server.server.URL)
	sub.EventType = EventUserMessagePostSend
	sub.IncludeContent = true
	m := newTestManager(t, sub)
	m.Append(
		HandlerFunc{HandlerName: "redactor", EventTypes: []EventType{EventUserMessagePostSend}, Fn: func(_ context.Context, ev *Event) (*Decision, error) {
			return &Decision{RewrittenOutput: "SAFE", Annotations: map[string]string{"redactor": "applied"}}, nil
		}},
		HandlerFunc{HandlerName: "observer", EventTypes: []EventType{EventUserMessagePostSend}, Fn: func(_ context.Context, ev *Event) (*Decision, error) {
			if ev.Content.Output != "SAFE" {
				t.Errorf("later handler saw original draft: %q", ev.Content.Output)
			}
			return &Decision{Annotations: map[string]string{"handler": "checked"}}, nil
		}},
	)
	ev := &Event{Type: EventUserMessagePostSend, TenantID: "tnt_1", Content: &EventContent{Output: "SECRET"}}
	decision, err := m.EmitOutputReview(context.Background(), ev)
	if err != nil || decision == nil || decision.RewrittenOutput != "SAFE" {
		t.Fatalf("earlier rewrite erased: %+v / %v", decision, err)
	}
	for _, key := range []string{"redactor", "handler", "subscription"} {
		if decision.Annotations[key] == "" {
			t.Errorf("annotation %q erased: %+v", key, decision.Annotations)
		}
	}
	if got := server.calls(); len(got) != 1 || got[0].Content.Output != "SAFE" {
		t.Fatalf("subscription saw unredacted draft: %+v", got)
	}
}

func TestWillReviewOutputIncludesNonMutatingAndInProcessPolicies(t *testing.T) {
	post := &Event{Type: EventUserMessagePostSend, TenantID: "tnt_1"}
	blocking := &Subscription{ID: "block", EventType: EventUserMessagePostSend, Mode: ModeBlocking, Enabled: true}
	observing := &Subscription{ID: "observe", EventType: EventUserMessagePostSend, Mode: ModeAsync, Enabled: true}
	if newTestManager(t, observing).WillReviewOutput(post) {
		t.Fatal("async-only observers require review")
	}
	if !newTestManager(t, blocking).WillReviewOutput(post) {
		t.Fatal("blocking policy without mutation did not require review")
	}
	m := newTestManager(t)
	m.Append(HandlerFunc{HandlerName: "trusted review", EventTypes: []EventType{EventUserMessagePostSend}})
	if !m.WillReviewOutput(post) {
		t.Fatal("in-process output policy did not require review")
	}
}

func TestValidateSubscriptionURLRejectsPrivateTargets(t *testing.T) {
	blocked := []string{
		"http://example.com/hook",        // plaintext
		"https://127.0.0.1/hook",         // loopback
		"https://169.254.169.254/latest", // cloud metadata
		"https://10.0.0.5/hook",          // private range
		"https://localhost/hook",
		"not-a-url",
	}
	for _, raw := range blocked {
		if err := ValidateSubscriptionURL(context.Background(), raw, false); err == nil {
			t.Errorf("ValidateSubscriptionURL(%q) allowed a target it must refuse", raw)
		}
	}
	if err := ValidateSubscriptionURL(context.Background(), "https://127.0.0.1/hook", true); err != nil {
		t.Errorf("allowPrivate should permit a loopback endpoint for local development: %v", err)
	}
}
