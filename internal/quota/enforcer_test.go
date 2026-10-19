package quota_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/earlgrey/piproxy/internal/quota"
)

type fakeCounterStore struct {
	connected   bool
	concurrency map[string]int64
	windows     map[string]int64
	daily       map[string]int64
	monthly     map[string]int64
	calls       int
}

type durableCounterStore struct {
	*fakeCounterStore
	failApply bool
	applied   map[string]bool
}

func newDurableCounterStore() *durableCounterStore {
	return &durableCounterStore{
		fakeCounterStore: newFakeCounterStore(),
		applied:          make(map[string]bool),
	}
}

func (s *durableCounterStore) ApplySpendSettlement(_ context.Context, counterKey, settlementID string, amountMillicents int64, _ time.Duration) (int64, error) {
	if s.failApply {
		return 0, errors.New("injected settlement failure")
	}
	values := s.daily
	if strings.Contains(counterKey, ":monthly:") {
		values = s.monthly
	}
	if !s.applied[settlementID] {
		values[counterKey] += amountMillicents
		s.applied[settlementID] = true
	}
	return values[counterKey], nil
}

func (s *durableCounterStore) EnsureSpendFloor(_ context.Context, counterKey, settlementID string, minimumMillicents int64, _ time.Duration) (int64, error) {
	if s.failApply {
		return 0, errors.New("injected settlement failure")
	}
	values := s.daily
	if strings.Contains(counterKey, ":monthly:") {
		values = s.monthly
	}
	if !s.applied[settlementID] {
		if values[counterKey] < minimumMillicents {
			values[counterKey] = minimumMillicents
		}
		s.applied[settlementID] = true
	}
	return values[counterKey], nil
}

type fakeSpendSettlementStore struct {
	rows map[string]quota.SpendSettlement
}

func newFakeSpendSettlementStore() *fakeSpendSettlementStore {
	return &fakeSpendSettlementStore{rows: make(map[string]quota.SpendSettlement)}
}

func (s *fakeSpendSettlementStore) EnqueueSpendSettlements(_ context.Context, settlements []quota.SpendSettlement) error {
	for _, settlement := range settlements {
		if _, exists := s.rows[settlement.ID]; !exists {
			s.rows[settlement.ID] = settlement
		}
	}
	return nil
}

func (s *fakeSpendSettlementStore) ListPendingSpendSettlements(_ context.Context, scopeType quota.ScopeType, scopeID string, limit int) ([]quota.SpendSettlement, error) {
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

func (s *fakeSpendSettlementStore) DeleteSpendSettlement(_ context.Context, id string) error {
	delete(s.rows, id)
	return nil
}

func newFakeCounterStore() *fakeCounterStore {
	return &fakeCounterStore{
		connected:   true,
		concurrency: make(map[string]int64),
		windows:     make(map[string]int64),
		daily:       make(map[string]int64),
		monthly:     make(map[string]int64),
	}
}

func (s *fakeCounterStore) IsConnected() bool { return s.connected }

func (s *fakeCounterStore) ConcurrencyIncr(_ context.Context, key string) (int64, error) {
	s.calls++
	s.concurrency[key]++
	return s.concurrency[key], nil
}

func (s *fakeCounterStore) ConcurrencyDecr(_ context.Context, key string) (int64, error) {
	s.calls++
	s.concurrency[key]--
	return s.concurrency[key], nil
}

func (s *fakeCounterStore) SlidingWindowAdd(_ context.Context, key string, _ time.Duration, amount int64) (int64, error) {
	s.calls++
	s.windows[key] += amount
	return s.windows[key], nil
}

func (s *fakeCounterStore) DailyAdd(_ context.Context, key string, amount int64) (int64, error) {
	s.calls++
	s.daily[key] += amount
	return s.daily[key], nil
}

func (s *fakeCounterStore) MonthlyAdd(_ context.Context, key string, amount int64) (int64, error) {
	s.calls++
	s.monthly[key] += amount
	return s.monthly[key], nil
}

func (s *fakeCounterStore) dailyWithSubstring(fragment string) int64 {
	for key, value := range s.daily {
		if strings.Contains(key, fragment) {
			return value
		}
	}
	return 0
}

func TestEnforcerTokenLimitsUseEstimatedTokenWeight(t *testing.T) {
	manager := quota.NewManager()
	manager.SetPolicies([]*quota.Policy{{
		ID:        "pol_test",
		TenantID:  "ten_test",
		ScopeType: quota.ScopeKey,
		ScopeID:   "key_test",
		Enabled:   true,
		Limits: map[quota.LimitType]int64{
			quota.LimitConcurrentRequests: 10,
			quota.LimitRequestsPerMinute:  10,
			quota.LimitTokensPerMinute:    100,
			quota.LimitTotalTokensPerDay:  1000,
		},
	}})

	store := newFakeCounterStore()
	enforcer := quota.NewEnforcer(manager, store, nil)

	if _, err := enforcer.CheckAndReserve(context.Background(), quota.ScopeKey, "key_test", "gpt-test", 60); err != nil {
		t.Fatalf("first reservation failed: %v", err)
	}
	_, err := enforcer.CheckAndReserve(context.Background(), quota.ScopeKey, "key_test", "gpt-test", 50)
	if err == nil {
		t.Fatal("expected token-per-minute quota error")
	}
	qerr, ok := err.(*quota.ExceededError)
	if !ok {
		t.Fatalf("expected ExceededError, got %T", err)
	}
	if qerr.Limit != quota.LimitTokensPerMinute {
		t.Fatalf("expected TPM limit, got %s", qerr.Limit)
	}

	var tpmTotal int64
	for key, total := range store.windows {
		if strings.Contains(key, ":tpm:") {
			tpmTotal = total
		}
	}
	if tpmTotal != 110 {
		t.Fatalf("expected TPM window to add token estimates to 110, got %d", tpmTotal)
	}
}

func TestDeferredSpendPersistsAndReplaysBeforeNextRequest(t *testing.T) {
	manager := quota.NewManager()
	manager.SetPolicies([]*quota.Policy{{
		ID:        "pol_deferred",
		ScopeType: quota.ScopeKey,
		ScopeID:   "key_deferred",
		Enabled:   true,
		Limits: map[quota.LimitType]int64{
			quota.LimitDailySpendCents: 1,
		},
	}})
	counters := newDurableCounterStore()
	outbox := newFakeSpendSettlementStore()
	enforcer := quota.NewEnforcer(manager, counters, nil)
	enforcer.SetSpendSettlementStore(outbox)

	resv, err := enforcer.CheckAndReserveDeferredSpend(context.Background(), quota.ScopeKey, "key_deferred", "gpt-test", 10)
	if err != nil {
		t.Fatalf("initial deferred reservation: %v", err)
	}
	counters.failApply = true
	if err := enforcer.Complete(context.Background(), resv, 10, int64(1000)); err == nil {
		t.Fatal("completion should report the Redis settlement failure")
	}
	if len(outbox.rows) == 0 {
		t.Fatal("failed charge was not retained in the durable outbox")
	}
	if got := counters.dailyWithSubstring(":spend_millicents:"); got != 0 {
		t.Fatalf("daily spend applied despite injected failure: %d", got)
	}

	counters.failApply = false
	_, err = enforcer.CheckAndReserveDeferredSpend(context.Background(), quota.ScopeKey, "key_deferred", "gpt-test", 10)
	if err == nil {
		t.Fatal("next request should replay the pending charge and be denied at the limit")
	}
	var exceeded *quota.ExceededError
	if !errors.As(err, &exceeded) || exceeded.Limit != quota.LimitDailySpendCents {
		t.Fatalf("next request error = %T %v, want daily spend ExceededError", err, err)
	}
	if len(outbox.rows) != 0 {
		t.Fatalf("applied outbox rows were not deleted: %#v", outbox.rows)
	}
	if got := counters.dailyWithSubstring(":spend_millicents:"); got != 1000 {
		t.Fatalf("replayed daily spend = %d, want 1000", got)
	}
}

func TestDeferredSpendReplaysAbsoluteLedgerFloorWithoutDoubleCharge(t *testing.T) {
	manager := quota.NewManager()
	manager.SetPolicies([]*quota.Policy{{
		ID: "pol_floor", ScopeType: quota.ScopeKey, ScopeID: "key_floor", Enabled: true,
		Limits: map[quota.LimitType]int64{quota.LimitDailySpendCents: 1},
	}})
	counters := newDurableCounterStore()
	outbox := newFakeSpendSettlementStore()
	key := "quota:daily:spend_millicents:api_key:key_floor:" + time.Now().UTC().Format("2006-01-02")
	counters.daily[key] = 400
	outbox.rows["floor_1"] = quota.SpendSettlement{
		ID: "floor_1", ScopeType: quota.ScopeKey, ScopeID: "key_floor", CounterKey: key,
		Limit: quota.LimitDailySpendCents, AmountMillicents: 1000,
		Operation: quota.SpendSettlementEnsureMinimum,
	}
	enforcer := quota.NewEnforcer(manager, counters, nil)
	enforcer.SetSpendSettlementStore(outbox)

	_, err := enforcer.CheckAndReserveDeferredSpend(context.Background(), quota.ScopeKey, "key_floor", "gpt-test", 10)
	if err == nil {
		t.Fatal("ledger floor at the limit should deny the next request")
	}
	if got := counters.daily[key]; got != 1000 {
		t.Fatalf("spend floor = %d, want 1000", got)
	}
	if len(outbox.rows) != 0 {
		t.Fatalf("applied floor remained pending: %#v", outbox.rows)
	}
}

func TestEnforcerDailyAndMonthlyLimitsUseEstimatedTokenWeight(t *testing.T) {
	manager := quota.NewManager()
	manager.SetPolicies([]*quota.Policy{{
		ID:        "pol_test",
		TenantID:  "ten_test",
		ScopeType: quota.ScopeKey,
		ScopeID:   "key_test",
		Enabled:   true,
		Limits: map[quota.LimitType]int64{
			quota.LimitConcurrentRequests:  10,
			quota.LimitRequestsPerMinute:   10,
			quota.LimitTokensPerMinute:     1000,
			quota.LimitTotalTokensPerDay:   100,
			quota.LimitTotalTokensPerMonth: 1000,
		},
	}})

	store := newFakeCounterStore()
	enforcer := quota.NewEnforcer(manager, store, nil)

	if _, err := enforcer.CheckAndReserve(context.Background(), quota.ScopeKey, "key_test", "gpt-test", 60); err != nil {
		t.Fatalf("first reservation failed: %v", err)
	}
	_, err := enforcer.CheckAndReserve(context.Background(), quota.ScopeKey, "key_test", "gpt-test", 50)
	if err == nil {
		t.Fatal("expected daily token quota error")
	}
	qerr, ok := err.(*quota.ExceededError)
	if !ok {
		t.Fatalf("expected ExceededError, got %T", err)
	}
	if qerr.Limit != quota.LimitTotalTokensPerDay {
		t.Fatalf("expected daily token limit, got %s", qerr.Limit)
	}

	var dailyTotal int64
	for _, total := range store.daily {
		dailyTotal = total
	}
	if dailyTotal != 110 {
		t.Fatalf("expected daily counter to add token estimates to 110, got %d", dailyTotal)
	}
}

func TestEnforcerMaxInputLimitDoesNotMutateCounters(t *testing.T) {
	manager := quota.NewManager()
	manager.SetPolicies([]*quota.Policy{{
		ID:        "pol_test",
		TenantID:  "ten_test",
		ScopeType: quota.ScopeKey,
		ScopeID:   "key_test",
		Enabled:   true,
		Limits: map[quota.LimitType]int64{
			quota.LimitMaxInputTokens: 10,
		},
	}})

	store := newFakeCounterStore()
	enforcer := quota.NewEnforcer(manager, store, nil)

	_, err := enforcer.CheckAndReserve(context.Background(), quota.ScopeKey, "key_test", "gpt-test", 20)
	if err == nil {
		t.Fatal("expected max input token quota error")
	}
	qerr, ok := err.(*quota.ExceededError)
	if !ok {
		t.Fatalf("expected ExceededError, got %T", err)
	}
	if qerr.Limit != quota.LimitMaxInputTokens {
		t.Fatalf("expected max input token limit, got %s", qerr.Limit)
	}
	if store.calls != 0 {
		t.Fatalf("expected no counter mutations before max-input rejection, got %d calls", store.calls)
	}
}

func TestEnforcerSpendLimitsUseEstimatedSpendMillicents(t *testing.T) {
	manager := quota.NewManager()
	manager.SetPolicies([]*quota.Policy{{
		ID:        "pol_test",
		TenantID:  "ten_test",
		ScopeType: quota.ScopeKey,
		ScopeID:   "key_test",
		Enabled:   true,
		Limits: map[quota.LimitType]int64{
			quota.LimitConcurrentRequests: 10,
			quota.LimitDailySpendCents:    1,
			quota.LimitMonthlySpendCents:  100,
		},
	}})

	store := newFakeCounterStore()
	enforcer := quota.NewEnforcer(manager, store, nil)

	if _, err := enforcer.CheckAndReserve(context.Background(), quota.ScopeKey, "key_test", "gpt-test", 10, 600); err != nil {
		t.Fatalf("first reservation failed: %v", err)
	}
	_, err := enforcer.CheckAndReserve(context.Background(), quota.ScopeKey, "key_test", "gpt-test", 10, 500)
	if err == nil {
		t.Fatal("expected daily spend quota error")
	}
	qerr, ok := err.(*quota.ExceededError)
	if !ok {
		t.Fatalf("expected ExceededError, got %T", err)
	}
	if qerr.Limit != quota.LimitDailySpendCents {
		t.Fatalf("expected daily spend limit, got %s", qerr.Limit)
	}

	var dailySpend int64
	for key, total := range store.daily {
		if strings.Contains(key, "spend_millicents") {
			dailySpend = total
		}
	}
	if dailySpend != 600 {
		t.Fatalf("expected denied spend reservation to be refunded to 600, got %d", dailySpend)
	}
}

func TestEnforcerRollbackRefundsSpendReservation(t *testing.T) {
	manager := quota.NewManager()
	manager.SetPolicies([]*quota.Policy{{
		ID:        "pol_test",
		TenantID:  "ten_test",
		ScopeType: quota.ScopeKey,
		ScopeID:   "key_test",
		Enabled:   true,
		Limits: map[quota.LimitType]int64{
			quota.LimitConcurrentRequests: 10,
			quota.LimitDailySpendCents:    100,
			quota.LimitMonthlySpendCents:  100,
		},
	}})

	store := newFakeCounterStore()
	enforcer := quota.NewEnforcer(manager, store, nil)

	resv, err := enforcer.CheckAndReserve(context.Background(), quota.ScopeKey, "key_test", "gpt-test", 10, 750)
	if err != nil {
		t.Fatalf("reservation failed: %v", err)
	}
	if err := enforcer.Rollback(context.Background(), resv); err != nil {
		t.Fatalf("rollback failed: %v", err)
	}

	for key, total := range store.daily {
		if strings.Contains(key, "spend_millicents") && total != 0 {
			t.Fatalf("expected daily spend refund to zero for %s, got %d", key, total)
		}
	}
	for key, total := range store.monthly {
		if strings.Contains(key, "spend_millicents") && total != 0 {
			t.Fatalf("expected monthly spend refund to zero for %s, got %d", key, total)
		}
	}
}

func TestEnforcerCompleteReconcilesSpendToActual(t *testing.T) {
	manager := quota.NewManager()
	manager.SetPolicies([]*quota.Policy{{
		ID:        "pol_test",
		TenantID:  "ten_test",
		ScopeType: quota.ScopeKey,
		ScopeID:   "key_test",
		Enabled:   true,
		Limits: map[quota.LimitType]int64{
			quota.LimitConcurrentRequests: 10,
			quota.LimitDailySpendCents:    100,
			quota.LimitMonthlySpendCents:  100,
		},
	}})

	store := newFakeCounterStore()
	enforcer := quota.NewEnforcer(manager, store, nil)

	resv, err := enforcer.CheckAndReserve(context.Background(), quota.ScopeKey, "key_test", "gpt-test", 10, 900)
	if err != nil {
		t.Fatalf("reservation failed: %v", err)
	}
	if err := enforcer.Complete(context.Background(), resv, 10, 250); err != nil {
		t.Fatalf("complete failed: %v", err)
	}

	for key, total := range store.daily {
		if strings.Contains(key, "spend_millicents") && total != 250 {
			t.Fatalf("expected daily spend to reconcile to 250 for %s, got %d", key, total)
		}
	}
	for key, total := range store.monthly {
		if strings.Contains(key, "spend_millicents") && total != 250 {
			t.Fatalf("expected monthly spend to reconcile to 250 for %s, got %d", key, total)
		}
	}
}

func TestEnforcerCompleteActualSpendOverCapDeniesNextRequest(t *testing.T) {
	manager := quota.NewManager()
	manager.SetPolicies([]*quota.Policy{{
		ID:        "pol_test",
		TenantID:  "ten_test",
		ScopeType: quota.ScopeKey,
		ScopeID:   "key_test",
		Enabled:   true,
		Limits: map[quota.LimitType]int64{
			quota.LimitConcurrentRequests: 10,
			quota.LimitDailySpendCents:    1,
			quota.LimitMonthlySpendCents:  100,
		},
	}})

	store := newFakeCounterStore()
	enforcer := quota.NewEnforcer(manager, store, nil)

	resv, err := enforcer.CheckAndReserve(context.Background(), quota.ScopeKey, "key_test", "gpt-test", 10, 700)
	if err != nil {
		t.Fatalf("reservation failed: %v", err)
	}
	err = enforcer.Complete(context.Background(), resv, 10, 1100)
	if err == nil {
		t.Fatal("expected reconciliation overage error")
	}
	var overage *quota.ReconciliationOverageError
	if !errors.As(err, &overage) {
		t.Fatalf("expected ReconciliationOverageError, got %T: %v", err, err)
	}
	if overage.Limit != quota.LimitDailySpendCents {
		t.Fatalf("overage limit = %s, want %s", overage.Limit, quota.LimitDailySpendCents)
	}

	var dailySpend int64
	for key, total := range store.daily {
		if strings.Contains(key, "spend_millicents") {
			dailySpend = total
		}
	}
	if dailySpend != 1100 {
		t.Fatalf("expected reconciled daily spend 1100, got %d", dailySpend)
	}

	_, err = enforcer.CheckAndReserve(context.Background(), quota.ScopeKey, "key_test", "gpt-test", 10, 1)
	if err == nil {
		t.Fatal("expected next request to be denied after reconciled overage")
	}
	qerr, ok := err.(*quota.ExceededError)
	if !ok {
		t.Fatalf("expected ExceededError, got %T: %v", err, err)
	}
	if qerr.Limit != quota.LimitDailySpendCents {
		t.Fatalf("denied limit = %s, want %s", qerr.Limit, quota.LimitDailySpendCents)
	}
}
