package dataplane_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/auth"
	"github.com/earlgrey/piproxy/internal/capture"
	"github.com/earlgrey/piproxy/internal/httpapi/dataplane"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	"github.com/earlgrey/piproxy/internal/providers"
	"github.com/earlgrey/piproxy/internal/quota"
	"github.com/earlgrey/piproxy/internal/secrets"
	"github.com/earlgrey/piproxy/internal/stream"
	"github.com/earlgrey/piproxy/internal/subscriptioncost"
	"github.com/earlgrey/piproxy/internal/usage"
)

type fixedRateResolver struct {
	card subscriptioncost.RateCard
}

func (r fixedRateResolver) Resolve(_, _ string) (subscriptioncost.RateCard, bool) {
	return r.card, r.card.USDPerMillionTokens > 0
}

type captureStore struct {
	ch chan *capture.Payload
}

type quotaCounterStore struct {
	daily       map[string]int64
	monthly     map[string]int64
	concurrency map[string]int64
	settled     map[string]bool
}

func newQuotaCounterStore() *quotaCounterStore {
	return &quotaCounterStore{
		daily:       map[string]int64{},
		monthly:     map[string]int64{},
		concurrency: map[string]int64{},
		settled:     map[string]bool{},
	}
}

func (s *quotaCounterStore) IsConnected() bool { return true }

func (s *quotaCounterStore) ConcurrencyIncr(ctx context.Context, key string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.concurrency[key]++
	return s.concurrency[key], nil
}

func (s *quotaCounterStore) ConcurrencyDecr(ctx context.Context, key string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.concurrency[key]--
	return s.concurrency[key], nil
}

func (s *quotaCounterStore) SlidingWindowAdd(context.Context, string, time.Duration, int64) (int64, error) {
	return 0, nil
}

func (s *quotaCounterStore) DailyAdd(_ context.Context, key string, amount int64) (int64, error) {
	s.daily[key] += amount
	return s.daily[key], nil
}

func (s *quotaCounterStore) MonthlyAdd(_ context.Context, key string, amount int64) (int64, error) {
	s.monthly[key] += amount
	return s.monthly[key], nil
}

func (s *quotaCounterStore) ApplySpendSettlement(_ context.Context, key, settlementID string, amount int64, _ time.Duration) (int64, error) {
	values := s.daily
	if len(key) >= len("quota:monthly:") && key[:len("quota:monthly:")] == "quota:monthly:" {
		values = s.monthly
	}
	if !s.settled[settlementID] {
		values[key] += amount
		s.settled[settlementID] = true
	}
	return values[key], nil
}

type quotaSettlementStore struct {
	rows map[string]quota.SpendSettlement
}

func newQuotaSettlementStore() *quotaSettlementStore {
	return &quotaSettlementStore{rows: map[string]quota.SpendSettlement{}}
}

func (s *quotaSettlementStore) EnqueueSpendSettlements(_ context.Context, settlements []quota.SpendSettlement) error {
	for _, settlement := range settlements {
		s.rows[settlement.ID] = settlement
	}
	return nil
}

func (s *quotaSettlementStore) ListPendingSpendSettlements(_ context.Context, scopeType quota.ScopeType, scopeID string, limit int) ([]quota.SpendSettlement, error) {
	result := make([]quota.SpendSettlement, 0)
	for _, settlement := range s.rows {
		if settlement.ScopeType == scopeType && settlement.ScopeID == scopeID {
			result = append(result, settlement)
			if len(result) == limit {
				break
			}
		}
	}
	return result, nil
}

func (s *quotaSettlementStore) DeleteSpendSettlement(_ context.Context, id string) error {
	delete(s.rows, id)
	return nil
}

func (s captureStore) Put(_ context.Context, payload *capture.Payload) error {
	s.ch <- payload
	return nil
}

func (s captureStore) Get(context.Context, string) (*capture.Payload, error) {
	return nil, nil
}

func TestApplyCredentialUsesSecretValueFallbacks(t *testing.T) {
	ctx := context.Background()
	store := secrets.NewCredentialStore(secrets.NewEnvStore("PROXY_TEST_"))

	os.Setenv("PROXY_TEST_PROVIDERS_ANTHROPIC_TOKEN", `{"bearer_token":"upstream-secret"}`)
	defer os.Unsetenv("PROXY_TEST_PROVIDERS_ANTHROPIC_TOKEN")

	headers := http.Header{}
	provider := &providers.Provider{
		Type:         providers.ProviderAnthropic,
		CredentialID: "providers/anthropic/token",
	}

	if err := dataplane.ApplyCredential(ctx, headers, provider, store); err != nil {
		t.Fatalf("ApplyCredential failed: %v", err)
	}

	if got := headers.Get("x-api-key"); got != "upstream-secret" {
		t.Fatalf("expected x-api-key to use bearer token fallback, got %q", got)
	}
}

func TestCapturePayloadSubmitsConversationIDAndBodies(t *testing.T) {
	store := captureStore{ch: make(chan *capture.Payload, 1)}
	recorder := capture.NewRecorder(store, nil, 1, 1, nil, zerolog.Nop())
	defer recorder.Close(context.Background())

	req, err := http.NewRequest("POST", "/openai/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set(capture.HeaderConversationID, "conv_123")

	provider := &providers.Provider{Type: providers.ProviderOpenAI}
	started := time.Now()
	completed := started.Add(time.Second)
	svcs := &dataplane.Services{
		Capture: recorder,
		CaptureConfig: dataplane.CaptureSettings{
			MaxPayloadBytes: 1000,
			RetentionDays:   30,
		},
	}

	dataplane.CapturePayload(req, svcs, provider, "gpt-4o", "success", "", false, []byte(`{"messages":[]}`), []byte(`{"ok":true}`), capture.ResponseFormatJSON, started, &completed)

	select {
	case payload := <-store.ch:
		if payload.ConversationID != "conv_123" {
			t.Fatalf("expected conversation id, got %q", payload.ConversationID)
		}
		if string(payload.RequestBody) != `{"messages":[]}` || string(payload.ResponseBody) != `{"ok":true}` {
			t.Fatalf("unexpected captured bodies: req=%q resp=%q", payload.RequestBody, payload.ResponseBody)
		}
		if payload.ExpiresAt == nil {
			t.Fatal("expected retention expiry")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for capture payload")
	}
}

func TestCapturePayloadAllowsMissingProvider(t *testing.T) {
	store := captureStore{ch: make(chan *capture.Payload, 1)}
	recorder := capture.NewRecorder(store, nil, 1, 1, nil, zerolog.Nop())
	defer recorder.Close(context.Background())

	req, err := http.NewRequest("POST", "/openai/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set(capture.HeaderSessionID, "sess_123")

	started := time.Now()
	svcs := &dataplane.Services{Capture: recorder}
	dataplane.CapturePayload(req, svcs, nil, "", "error", "bad_request", false, []byte(`{bad json`), []byte("invalid request"), capture.ResponseFormatText, started, nil)

	select {
	case payload := <-store.ch:
		if payload.Provider != "" {
			t.Fatalf("expected empty provider, got %q", payload.Provider)
		}
		if payload.ConversationID != "sess_123" {
			t.Fatalf("expected session id, got %q", payload.ConversationID)
		}
		if string(payload.RequestBody) != `{bad json` {
			t.Fatalf("unexpected request body: %q", payload.RequestBody)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for capture payload")
	}
}

func TestCheckQuotaAppliesTenantUserAndKeyScopes(t *testing.T) {
	manager := quota.NewManager()
	manager.SetPolicies([]*quota.Policy{
		{
			ID:        "tenant-cap",
			TenantID:  "tn_1",
			ScopeType: quota.ScopeTenant,
			ScopeID:   "tn_1",
			Enabled:   true,
			Limits: map[quota.LimitType]int64{
				quota.LimitDailySpendCents:    2,
				quota.LimitMonthlySpendCents:  100,
				quota.LimitConcurrentRequests: 10,
			},
		},
		{
			ID:        "user-cap",
			TenantID:  "tn_1",
			ScopeType: quota.ScopeUser,
			ScopeID:   "usr_1",
			Enabled:   true,
			Limits: map[quota.LimitType]int64{
				quota.LimitDailySpendCents:    100,
				quota.LimitMonthlySpendCents:  100,
				quota.LimitConcurrentRequests: 10,
			},
		},
		{
			ID:        "key-cap",
			TenantID:  "tn_1",
			ScopeType: quota.ScopeKey,
			ScopeID:   "key_1",
			Enabled:   true,
			Limits: map[quota.LimitType]int64{
				quota.LimitDailySpendCents:    100,
				quota.LimitMonthlySpendCents:  100,
				quota.LimitConcurrentRequests: 10,
			},
		},
	})
	store := newQuotaCounterStore()
	enforcer := quota.NewEnforcer(manager, store, nil)
	svcs := &dataplane.Services{Quota: enforcer}

	req, err := http.NewRequest("POST", "/openai/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req = req.WithContext(middleware.WithAuthContext(req.Context(), &auth.Context{
		TenantID: "tn_1",
		UserID:   "usr_1",
		KeyID:    "key_1",
	}))

	resv, err := dataplane.CheckQuota(req, svcs, nil, "gpt-4o", 10, 10_000)
	if err != nil {
		t.Fatalf("first reservation failed: %v", err)
	}
	dataplane.CompleteQuotaWithUsage(req, svcs, resv, nil, "gpt-4o", 10, 10_000)

	_, err = dataplane.CheckQuota(req, svcs, nil, "gpt-4o", 10, 10_000)
	if err == nil {
		t.Fatal("expected tenant spend cap to deny second request")
	}
	qerr, ok := err.(*quota.ExceededError)
	if !ok {
		t.Fatalf("expected quota exceeded error, got %T", err)
	}
	if qerr.Scope != quota.ScopeTenant || qerr.Limit != quota.LimitDailySpendCents {
		t.Fatalf("expected tenant daily spend denial, got scope=%s limit=%s", qerr.Scope, qerr.Limit)
	}
}

// An embedded nil store panics on any counter access, proving that exempt
// requests neither check nor mutate any rate, token, concurrency or spend counter.
type untouchedQuotaStore struct{ quota.CounterStore }

func TestCodexQuotaDisabledSkipsChecksAndSettlement(t *testing.T) {
	manager := quota.NewManager()
	manager.SetPolicies([]*quota.Policy{{
		ID: "input-cap", TenantID: "tn_1", ScopeType: quota.ScopeKey, ScopeID: "key_1", Enabled: true,
		Limits: map[quota.LimitType]int64{quota.LimitMaxInputTokens: 1},
	}})
	svcs := &dataplane.Services{
		Quota:              quota.NewEnforcer(manager, untouchedQuotaStore{}, nil),
		CodexQuotaDisabled: true,
	}
	req := httptest.NewRequest("POST", "/codex/responses", nil)
	req = req.WithContext(middleware.WithAuthContext(req.Context(), &auth.Context{
		TenantID: "tn_1", UserID: "usr_1", KeyID: "key_1",
	}))
	provider := &providers.Provider{Type: providers.ProviderOpenAICodexResponses, Billing: providers.BillingConfig{Mode: "subscription"}}
	// Missing subscription prices must not block the exempt request either.
	resv, err := dataplane.CheckQuota(req, svcs, provider, "gpt-test", 100, 100)
	if err != nil || resv != nil {
		t.Fatalf("exempt request: reservation=%v error=%v", resv, err)
	}
	dataplane.CompleteQuotaWithUsage(req, svcs, resv, provider, "gpt-test", 100, 100)
	dataplane.RollbackQuota(req, svcs, resv)

	provider.Billing.Mode = "metered"
	for _, providerType := range []providers.ProviderType{
		providers.ProviderOpenAI, providers.ProviderOpenAIResponses, providers.ProviderAnthropic,
	} {
		provider.Type = providerType
		if _, err := dataplane.CheckQuota(req, svcs, provider, "gpt-test", 100, 100); err == nil {
			t.Fatalf("quota disabled unexpectedly for %s", providerType)
		}
	}
	provider.Type = providers.ProviderOpenAICodexResponses
	svcs.CodexQuotaDisabled = false
	if _, err := dataplane.CheckQuota(req, svcs, provider, "gpt-test", 100, 100); err == nil {
		t.Fatal("re-enabling Codex quota did not restore input limit")
	}
}

func TestRollbackQuotaReleasesAllScopesAfterRequestCancellation(t *testing.T) {
	manager := quota.NewManager()
	store := newQuotaCounterStore()
	enforcer := quota.NewEnforcer(manager, store, nil)
	svcs := &dataplane.Services{Quota: enforcer}

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "POST", "/codex/responses", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req = req.WithContext(middleware.WithAuthContext(req.Context(), &auth.Context{
		TenantID: "tn_cancelled",
		UserID:   "usr_cancelled",
		KeyID:    "key_cancelled",
	}))

	resv, err := dataplane.CheckQuota(req, svcs, nil, "gpt-test", 10, 10)
	if err != nil {
		t.Fatalf("reserve quota: %v", err)
	}
	cancel()
	dataplane.RollbackQuota(req, svcs, resv)

	for key, count := range store.concurrency {
		if count != 0 {
			t.Errorf("concurrency counter %s = %d after cancelled request rollback, want 0", key, count)
		}
	}
}

func TestSubscriptionBillingUsesDailyRateCard(t *testing.T) {
	provider := &providers.Provider{
		ID: "codex-account",
		Billing: providers.BillingConfig{
			Mode:             "subscription",
			MonthlyCostCents: 600,
		},
	}
	svcs := &dataplane.Services{SubscriptionRates: fixedRateResolver{card: subscriptioncost.RateCard{
		ID: 42, USDPerMillionTokens: 0.5,
	}}}
	cost, rateCardID, basis, explicit := dataplane.ProviderCostTableForModel(svcs, provider, "new-model")
	if !explicit {
		t.Fatal("subscription rate card must be resolved")
	}
	if cost.InputPricePerMillion != 0.5 || cost.OutputPricePerMillion != 0.5 {
		t.Fatalf("unexpected subscription rate: %#v", cost)
	}
	if rateCardID != 42 || basis != "subscription_rate_card" {
		t.Fatalf("unexpected rate metadata: id=%d basis=%s", rateCardID, basis)
	}
	if got := dataplane.EstimateSpendMillicents(svcs, provider, "new-model", 1_000_000, 1_000_000); got != 100_000 {
		t.Fatalf("subscription charge = %d millicents, want 100000", got)
	}
	if got := dataplane.EstimateSpendMillicentsWithCache(svcs, provider, "new-model", usage.TokenUsage{TotalTokens: 2_000_000}); got != 100_000 {
		t.Fatalf("total-token-only subscription charge = %d millicents, want 100000", got)
	}
}

func TestSubscriptionSpendIsChargedAfterCompletion(t *testing.T) {
	manager := quota.NewManager()
	manager.SetPolicies([]*quota.Policy{{
		ID: "key-cap", TenantID: "tn_1", ScopeType: quota.ScopeKey, ScopeID: "key_1", Enabled: true,
		Limits: map[quota.LimitType]int64{
			quota.LimitDailySpendCents:    1,
			quota.LimitMonthlySpendCents:  100,
			quota.LimitConcurrentRequests: 10,
		},
	}})
	store := newQuotaCounterStore()
	provider := &providers.Provider{ID: "codex-account", Billing: providers.BillingConfig{Mode: "subscription", MonthlyCostCents: 20000}}
	enforcer := quota.NewEnforcer(manager, store, nil)
	enforcer.SetSpendSettlementStore(newQuotaSettlementStore())
	svcs := &dataplane.Services{
		Quota:             enforcer,
		SubscriptionRates: fixedRateResolver{card: subscriptioncost.RateCard{ID: 42, USDPerMillionTokens: 1}},
	}
	req, err := http.NewRequest("POST", "/codex/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	req = req.WithContext(middleware.WithAuthContext(req.Context(), &auth.Context{KeyID: "key_1"}))
	resv, err := dataplane.CheckQuota(req, svcs, provider, "gpt-codex", 1, 1)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if got := spendCounterTotal(store.daily); got != 0 {
		t.Fatalf("spend was reserved before completion: %d", got)
	}
	dataplane.CompleteQuotaWithUsage(req, svcs, resv, provider, "gpt-codex", 10_000, 0)
	if got := spendCounterTotal(store.daily); got != 1_000 {
		t.Fatalf("completed charge = %d millicents, want 1000", got)
	}
	if _, err := dataplane.CheckQuota(req, svcs, provider, "gpt-codex", 1, 1); err == nil {
		t.Fatal("next request should be denied at the spend limit")
	}
}

func spendCounterTotal(values map[string]int64) int64 {
	var total int64
	for key, value := range values {
		if len(key) >= 20 && value > 0 {
			total += value
		}
	}
	return total
}

func TestMeteredBillingUsesProviderRates(t *testing.T) {
	provider := &providers.Provider{
		Billing: providers.BillingConfig{
			Mode:                      "metered",
			InputPricePerMillion:      1.25,
			OutputPricePerMillion:     4.5,
			CacheReadPricePerMillion:  0.1,
			CacheWritePricePerMillion: 2,
		},
	}
	cost, explicit := dataplane.ProviderCostTable(provider)
	if !explicit {
		t.Fatal("metered billing must be an explicit cost override")
	}
	if cost.InputPricePerMillion != 1.25 || cost.OutputPricePerMillion != 4.5 ||
		cost.CacheReadPricePerMillion != 0.1 || cost.CacheWritePricePerMillion != 2 {
		t.Fatalf("unexpected metered cost table: %#v", cost)
	}
}

func TestObservedSubscriptionNeverExtrapolatesLegacyRate(t *testing.T) {
	provider := &providers.Provider{ID: "codex", Billing: providers.BillingConfig{Mode: "subscription", AllocationMethod: subscriptioncost.ObservedDelta}}
	svcs := &dataplane.Services{SubscriptionRates: fixedRateResolver{card: subscriptioncost.RateCard{ID: 42, USDPerMillionTokens: 123.403467637}}}
	cost, id, basis, explicit := dataplane.ProviderCostTableForModel(svcs, provider, "gpt-codex")
	if !explicit || id != 0 || basis != subscriptioncost.PendingBasis || cost.InputPricePerMillion != 0 {
		t.Fatalf("legacy rate leaked into observed accounting: %+v %d %s", cost, id, basis)
	}
	if got := dataplane.EstimateSpendMillicentsWithCache(svcs, provider, "gpt-codex", usage.TokenUsage{TotalTokens: 100_000_000}); got != 0 {
		t.Fatalf("pending request charged %d", got)
	}
}

// recorderGapSink collects gap markers submitted through a Recorder.
type recorderGapSink struct {
	ch chan capture.Gap
}

func (s *recorderGapSink) Record(gap capture.Gap) bool {
	s.ch <- gap
	return true
}

func TestCapturePayloadWithStreamCaptureMarksTruncationAndGap(t *testing.T) {
	store := captureStore{ch: make(chan *capture.Payload, 1)}
	gapSink := &recorderGapSink{ch: make(chan capture.Gap, 1)}
	recorder := capture.NewRecorder(store, gapSink, 1, 1, nil, zerolog.Nop())
	defer recorder.Close(context.Background())

	req, err := http.NewRequest("POST", "/openai/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Request-ID", "req_stream_gap_test")
	req.Header.Set(capture.HeaderConversationID, "conv_stream_gap")
	req = req.WithContext(context.WithValue(req.Context(), middleware.RequestIDKey, "req_stream_gap_test"))

	sc := dataplane.NewStreamCapture(8)
	sc.Append(&stream.Event{Data: "aaaaaaaaaa", Raw: []byte("aaaaaaaaaa")})
	if !sc.Truncated() {
		t.Fatal("test setup: expected the stream capture to be truncated")
	}

	svcs := &dataplane.Services{Capture: recorder}
	started := time.Now()
	dataplane.CapturePayload(req, svcs, nil, "deepseek-chat", "success", "", true,
		[]byte(`{"messages":[]}`), sc.Bytes(), capture.ResponseFormatSSE, started, nil,
		dataplane.WithStreamCapture(sc))

	select {
	case gap := <-gapSink.ch:
		if gap.Reason != capture.GapReasonResponseTruncated {
			t.Fatalf("expected response_truncated gap, got %q", gap.Reason)
		}
		if gap.ConversationID != "conv_stream_gap" {
			t.Fatalf("expected gap conversation id, got %q", gap.ConversationID)
		}
		if gap.RequestID == "" {
			t.Fatal("expected gap to carry the request id")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for capture gap")
	}

	select {
	case payload := <-store.ch:
		if !payload.ResponseTruncated {
			t.Fatal("expected payload response_truncated flag from stream capture")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for capture payload")
	}
}

func TestCapturePayloadWithResponseTruncatedOption(t *testing.T) {
	store := captureStore{ch: make(chan *capture.Payload, 1)}
	gapSink := &recorderGapSink{ch: make(chan capture.Gap, 1)}
	recorder := capture.NewRecorder(store, gapSink, 1, 1, nil, zerolog.Nop())
	defer recorder.Close(context.Background())

	req, err := http.NewRequest("POST", "/openai/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	svcs := &dataplane.Services{Capture: recorder}
	started := time.Now()
	dataplane.CapturePayload(req, svcs, nil, "deepseek-chat", "success", "", false,
		[]byte(`{}`), []byte(`{}`), capture.ResponseFormatJSON, started, nil,
		dataplane.WithResponseTruncated(true))

	select {
	case payload := <-store.ch:
		if !payload.ResponseTruncated {
			t.Fatal("expected response_truncated flag from option")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for capture payload")
	}
	// The plain flag option must not emit a gap marker on its own.
	select {
	case gap := <-gapSink.ch:
		t.Fatalf("unexpected gap marker: %+v", gap)
	case <-time.After(100 * time.Millisecond):
	}
}
