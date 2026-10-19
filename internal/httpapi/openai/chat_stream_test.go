package openai_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/auth"
	"github.com/earlgrey/piproxy/internal/httpapi/dataplane"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	"github.com/earlgrey/piproxy/internal/httpapi/openai"
	"github.com/earlgrey/piproxy/internal/providers"
	"github.com/earlgrey/piproxy/internal/quota"
	"github.com/earlgrey/piproxy/internal/rewrite"
	"github.com/earlgrey/piproxy/internal/upstream"
	usagepkg "github.com/earlgrey/piproxy/internal/usage"
)

// streamCounterStore records daily counter mutations so tests can distinguish
// settlement of consumed stream tokens from a full reservation rollback.
type streamCounterStore struct {
	daily      map[string]int64
	sawRefund  bool // a full-reservation refund == a rollback was applied
	reserveAmt int64
	reserved   bool
}

func newStreamCounterStore() *streamCounterStore {
	return &streamCounterStore{daily: make(map[string]int64)}
}

func (s *streamCounterStore) IsConnected() bool { return true }

func (s *streamCounterStore) ConcurrencyIncr(context.Context, string) (int64, error) { return 1, nil }
func (s *streamCounterStore) ConcurrencyDecr(context.Context, string) (int64, error) { return 0, nil }

func (s *streamCounterStore) SlidingWindowAdd(context.Context, string, time.Duration, int64) (int64, error) {
	return 1, nil
}

func (s *streamCounterStore) DailyAdd(_ context.Context, key string, amount int64) (int64, error) {
	if strings.Contains(key, "spend_millicents") {
		if !s.reserved {
			// Initial reservation made by CheckAndReserve.
			s.reserved = true
			s.reserveAmt = amount
		} else if amount == -s.reserveAmt {
			// Rollback refunds exactly the reserved amount; a Complete reconcile
			// applies only the (actual-estimated) delta, which differs unless the
			// actual spend is zero (never the case for a costed model + tokens).
			s.sawRefund = true
		}
	}
	s.daily[key] += amount
	return s.daily[key], nil
}

func (s *streamCounterStore) MonthlyAdd(_ context.Context, key string, amount int64) (int64, error) {
	s.daily[key] += amount
	return s.daily[key], nil
}

// newStreamQuotaServices builds a Services whose quota enforcer is backed by the
// supplied fake store, with a generous daily spend limit so a spend reservation
// is created (and thus refund/complete becomes observable) without ever denying
// the request. The usage recorder is drainable so tests can assert the recorded
// status.
func newStreamQuotaServices(store *streamCounterStore) (dataplane.Services, *usagepkg.Recorder) {
	manager := quota.NewManager()
	manager.SetPolicies([]*quota.Policy{{
		ID:        "pol_stream",
		TenantID:  "ten_stream",
		ScopeType: quota.ScopeTenant,
		ScopeID:   "ten_stream",
		Enabled:   true,
		Limits: map[quota.LimitType]int64{
			quota.LimitDailySpendCents: 1_000_000, // huge: never denies
		},
	}})
	enforcer := quota.NewEnforcer(manager, store, nil)
	recorder := usagepkg.NewRecorder(usagepkg.NewCostTable(), nil, nil, 16, zerolog.Nop())
	return dataplane.Services{
		Quota:     enforcer,
		Usage:     recorder,
		CostTable: usagepkg.NewCostTable(),
	}, recorder
}

// withTenantAuth attaches an auth context so ResolveQuotaScopes yields a tenant
// scope (otherwise no reservation is made and there is nothing to roll back).
func withTenantAuth(r *http.Request) *http.Request {
	return r.WithContext(middleware.WithAuthContext(r.Context(), &auth.Context{
		TenantID: "ten_stream",
		Scopes:   []string{"*"},
	}))
}

func newStreamingHandler(t *testing.T, services dataplane.Services, sseBody string) *openai.ChatHandler {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local listener unavailable: %v", err)
	}
	upstreamServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sseBody))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Returning here closes the connection => the relay sees a plain EOF.
	}))
	upstreamServer.Listener = listener
	upstreamServer.Start()
	t.Cleanup(upstreamServer.Close)

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
		services,
	)
}

// drainRecordedStatus returns the status/errorClass of the single usage event
// recorded by the handler. It fails the test if none is queued.
func drainRecordedStatus(t *testing.T, recorder *usagepkg.Recorder) (string, string) {
	t.Helper()
	recorder.Close()
	select {
	case ev, ok := <-recorder.Events():
		if !ok {
			t.Fatal("no usage event recorded")
		}
		return ev.Status, ev.ErrorClass
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for usage event")
		return "", ""
	}
}

func TestChatStreamTruncatedSettlesPartialUsageAndRecordsError(t *testing.T) {
	store := newStreamCounterStore()
	services, recorder := newStreamQuotaServices(store)

	// SSE body that ends at EOF WITHOUT a `data: [DONE]` line: a truncated stream.
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n"
	handler := newStreamingHandler(t, services, body)

	req := withTenantAuth(httptest.NewRequest("POST", "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","messages":[],"stream":true}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.Handle(rec, req)

	if store.sawRefund {
		t.Fatalf("truncated stream must settle consumed tokens, not roll back spend, daily=%v", store.daily)
	}
	status, errClass := drainRecordedStatus(t, recorder)
	if status != "error" || errClass != "stream_truncated" {
		t.Fatalf("truncated stream usage = %q/%q, want error/stream_truncated", status, errClass)
	}
}

func TestChatStreamDoneCompletesAsSuccess(t *testing.T) {
	store := newStreamCounterStore()
	services, recorder := newStreamQuotaServices(store)

	// A well-formed stream that ends with the [DONE] terminal sentinel.
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n" +
		"data: [DONE]\n\n"
	handler := newStreamingHandler(t, services, body)

	req := withTenantAuth(httptest.NewRequest("POST", "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","messages":[],"stream":true}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.Handle(rec, req)

	if store.sawRefund {
		t.Fatalf("completed stream must NOT refund the reservation, daily=%v", store.daily)
	}
	status, errClass := drainRecordedStatus(t, recorder)
	if status != "success" || errClass != "" {
		t.Fatalf("completed stream usage = %q/%q, want success/empty", status, errClass)
	}
}

func TestChatCancelledRequestRecordsClientClosed(t *testing.T) {
	store := newStreamCounterStore()
	services, recorder := newStreamQuotaServices(store)
	handler := newStreamingHandler(t, services, `{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)

	baseReq := httptest.NewRequest("POST", "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","messages":[]}`))
	ctx, cancel := context.WithCancel(baseReq.Context())
	cancel()
	req := withTenantAuth(baseReq.WithContext(ctx))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.Handle(rec, req)

	if rec.Code != dataplane.StatusClientClosedRequest {
		t.Fatalf("status = %d, want %d", rec.Code, dataplane.StatusClientClosedRequest)
	}
	if !store.sawRefund {
		t.Fatalf("client-closed request must roll back the reservation, daily=%v", store.daily)
	}
	status, errClass := drainRecordedStatus(t, recorder)
	if status != "error" || errClass != dataplane.ErrorClassClientClosed {
		t.Fatalf("cancelled request usage = %q/%q, want error/%s", status, errClass, dataplane.ErrorClassClientClosed)
	}
}

// TestChatStreamDonePreservesUsageEvent guards the [DONE] detection from
// breaking the existing usage-event handling: a stream carrying an explicit
// usage chunk plus [DONE] must still record the upstream-reported (non
// estimated) tokens.
func TestChatStreamDonePreservesUsageEvent(t *testing.T) {
	store := newStreamCounterStore()
	services, recorder := newStreamQuotaServices(store)

	body := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"total_tokens\":18}}\n\n" +
		"data: [DONE]\n\n"
	handler := newStreamingHandler(t, services, body)

	req := withTenantAuth(httptest.NewRequest("POST", "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4","messages":[],"stream":true}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.Handle(rec, req)

	recorder.Close()
	ev := <-recorder.Events()
	if ev.Status != "success" {
		t.Fatalf("status = %q, want success", ev.Status)
	}
	if ev.EstimatedUsage {
		t.Fatalf("usage should come from the upstream usage chunk, not be estimated")
	}
	if ev.TotalTokens != 18 {
		t.Fatalf("total tokens = %d, want 18 (from usage chunk)", ev.TotalTokens)
	}
}
