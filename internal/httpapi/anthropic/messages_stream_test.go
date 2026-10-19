package anthropic_test

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
	"github.com/earlgrey/piproxy/internal/httpapi/anthropic"
	"github.com/earlgrey/piproxy/internal/httpapi/dataplane"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
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
	sawRefund  bool
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

func withTenantAuth(r *http.Request) *http.Request {
	return r.WithContext(middleware.WithAuthContext(r.Context(), &auth.Context{
		TenantID: "ten_stream",
		Scopes:   []string{"*"},
	}))
}

func newStreamingHandler(t *testing.T, services dataplane.Services, sseBody string) *anthropic.MessagesHandler {
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
	return anthropic.NewMessagesHandler(
		upstream.NewClient(upstream.Config{Timeout: 2 * time.Second, MaxConnsPerHost: 10, MaxIdleConns: 10}),
		rewrite.NewRequestRewriter(registry),
		registry,
		services,
	)
}

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

func newMessagesRequest() *http.Request {
	req := withTenantAuth(httptest.NewRequest("POST", "/anthropic/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4","messages":[],"max_tokens":16,"stream":true}`)))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestMessagesStreamTruncatedSettlesPartialUsageAndRecordsError(t *testing.T) {
	store := newStreamCounterStore()
	services, recorder := newStreamQuotaServices(store)

	// SSE body that ends at EOF WITHOUT a message_stop event: a truncated stream.
	body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hi\"}}\n\n"
	handler := newStreamingHandler(t, services, body)

	rec := httptest.NewRecorder()
	handler.Handle(rec, newMessagesRequest())

	if store.sawRefund {
		t.Fatalf("truncated stream must settle consumed tokens, not roll back spend, daily=%v", store.daily)
	}
	recorder.Close()
	event := <-recorder.Events()
	if event.Status != "error" || event.ErrorClass != "stream_truncated" {
		t.Fatalf("truncated stream usage = %q/%q, want error/stream_truncated", event.Status, event.ErrorClass)
	}
	if event.OutputTokens <= 0 || !event.EstimatedUsage {
		t.Fatalf("partial content was not estimated for settlement: output=%d estimated=%v", event.OutputTokens, event.EstimatedUsage)
	}
}

func TestMessagesStreamMessageStopCompletesAsSuccess(t *testing.T) {
	store := newStreamCounterStore()
	services, recorder := newStreamQuotaServices(store)

	// A well-formed stream that ends with the message_stop terminal event.
	body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	handler := newStreamingHandler(t, services, body)

	rec := httptest.NewRecorder()
	handler.Handle(rec, newMessagesRequest())

	if store.sawRefund {
		t.Fatalf("completed stream must NOT refund the reservation, daily=%v", store.daily)
	}
	status, errClass := drainRecordedStatus(t, recorder)
	if status != "success" || errClass != "" {
		t.Fatalf("completed stream usage = %q/%q, want success/empty", status, errClass)
	}
}

// TestMessagesStreamMessageStopDataOnlyCompletes covers an upstream that omits
// the SSE `event: message_stop` line and carries the terminal marker only in the
// JSON `data:` payload. The dual detection (event OR data substring) must still
// treat it as a successful completion.
func TestMessagesStreamMessageStopDataOnlyCompletes(t *testing.T) {
	store := newStreamCounterStore()
	services, recorder := newStreamQuotaServices(store)

	body := "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10}}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	handler := newStreamingHandler(t, services, body)

	rec := httptest.NewRecorder()
	handler.Handle(rec, newMessagesRequest())

	if store.sawRefund {
		t.Fatalf("data-only message_stop must NOT refund, daily=%v", store.daily)
	}
	status, _ := drainRecordedStatus(t, recorder)
	if status != "success" {
		t.Fatalf("data-only message_stop usage status = %q, want success", status)
	}
}
