package quota_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/earlgrey/piproxy/internal/quota"
)

// errInjectingStore is a CounterStore that reports IsConnected() == true but can
// be configured to return an error from any individual counter op, letting us
// exercise the fail-closed-on-counter-error paths (distinct from the
// disconnected paths). It also supports flipping a "disconnected" flag.
type errInjectingStore struct {
	connected bool

	failConcurrencyIncr bool
	failSlidingWindow   bool
	failDailyAdd        bool
	failMonthlyAdd      bool

	concurrency map[string]int64
	windows     map[string]int64
	daily       map[string]int64
	monthly     map[string]int64
}

func newErrInjectingStore() *errInjectingStore {
	return &errInjectingStore{
		connected:   true,
		concurrency: make(map[string]int64),
		windows:     make(map[string]int64),
		daily:       make(map[string]int64),
		monthly:     make(map[string]int64),
	}
}

var errInjected = errors.New("injected counter failure")

func (s *errInjectingStore) IsConnected() bool { return s.connected }

func (s *errInjectingStore) ConcurrencyIncr(_ context.Context, key string) (int64, error) {
	if s.failConcurrencyIncr {
		return 0, errInjected
	}
	s.concurrency[key]++
	return s.concurrency[key], nil
}

func (s *errInjectingStore) ConcurrencyDecr(_ context.Context, key string) (int64, error) {
	s.concurrency[key]--
	if s.concurrency[key] <= 0 {
		delete(s.concurrency, key)
		return 0, nil
	}
	return s.concurrency[key], nil
}

func (s *errInjectingStore) SlidingWindowAdd(_ context.Context, key string, _ time.Duration, amount int64) (int64, error) {
	if s.failSlidingWindow {
		return 0, errInjected
	}
	s.windows[key] += amount
	return s.windows[key], nil
}

func (s *errInjectingStore) DailyAdd(_ context.Context, key string, amount int64) (int64, error) {
	if s.failDailyAdd {
		return 0, errInjected
	}
	s.daily[key] += amount
	return s.daily[key], nil
}

func (s *errInjectingStore) MonthlyAdd(_ context.Context, key string, amount int64) (int64, error) {
	if s.failMonthlyAdd {
		return 0, errInjected
	}
	s.monthly[key] += amount
	return s.monthly[key], nil
}

func (s *errInjectingStore) concurrencyTotal() int64 {
	var total int64
	for _, v := range s.concurrency {
		total += v
	}
	return total
}

func (s *errInjectingStore) dailyWithSubstr(sub string) int64 {
	for key, v := range s.daily {
		if strings.Contains(key, sub) {
			return v
		}
	}
	return 0
}

func policyWithLimits(limits map[quota.LimitType]int64) *quota.Manager {
	m := quota.NewManager()
	m.SetPolicies([]*quota.Policy{{
		ID:        "pol_fc",
		TenantID:  "ten_fc",
		ScopeType: quota.ScopeKey,
		ScopeID:   "key_fc",
		Enabled:   true,
		Limits:    limits,
	}})
	return m
}

// --- Disconnected Redis: enforced dimensions fail closed -------------------

func TestEnforcer_FailsClosed_WhenRedisDisconnected_Spend(t *testing.T) {
	manager := policyWithLimits(map[quota.LimitType]int64{
		quota.LimitDailySpendCents: 100,
	})
	store := newErrInjectingStore()
	store.connected = false
	enforcer := quota.NewEnforcer(manager, store, nil)

	resv, err := enforcer.CheckAndReserve(context.Background(), quota.ScopeKey, "key_fc", "gpt", 0, 500)
	if err == nil {
		t.Fatal("expected fail-closed denial with Redis disconnected and spend limit set")
	}
	if resv != nil {
		t.Fatalf("expected nil reservation on denial, got %+v", resv)
	}
	qerr, ok := err.(*quota.ExceededError)
	if !ok {
		t.Fatalf("expected ExceededError, got %T: %v", err, err)
	}
	if qerr.Limit != quota.LimitDailySpendCents {
		t.Fatalf("expected daily spend limit, got %s", qerr.Limit)
	}
}

func TestEnforcer_FailsClosed_RPM_WhenDisconnected(t *testing.T) {
	manager := policyWithLimits(map[quota.LimitType]int64{
		quota.LimitRequestsPerMinute: 10,
	})
	store := newErrInjectingStore()
	store.connected = false
	enforcer := quota.NewEnforcer(manager, store, nil)

	// No concurrency/spend limit configured, so the first enforced limit is RPM,
	// which must deny rather than fabricate a (0, nil) success.
	if _, err := enforcer.CheckAndReserve(context.Background(), quota.ScopeKey, "key_fc", "gpt", 0); err == nil {
		t.Fatal("expected RPM fail-closed denial with Redis disconnected")
	}
}

func TestEnforcer_FailsClosed_DailyTokens_WhenDisconnected(t *testing.T) {
	manager := policyWithLimits(map[quota.LimitType]int64{
		quota.LimitTotalTokensPerDay: 500,
	})
	store := newErrInjectingStore()
	store.connected = false
	enforcer := quota.NewEnforcer(manager, store, nil)

	_, err := enforcer.CheckAndReserve(context.Background(), quota.ScopeKey, "key_fc", "gpt", 100)
	if err == nil {
		t.Fatal("expected daily-token fail-closed denial with Redis disconnected")
	}
	qerr, ok := err.(*quota.ExceededError)
	if !ok {
		t.Fatalf("expected ExceededError, got %T: %v", err, err)
	}
	if qerr.Limit != quota.LimitTotalTokensPerDay {
		t.Fatalf("expected daily token limit, got %s", qerr.Limit)
	}
}

func TestEnforcer_FailsClosed_MonthlyTokens_WhenDisconnected(t *testing.T) {
	manager := policyWithLimits(map[quota.LimitType]int64{
		quota.LimitTotalTokensPerMonth: 500,
	})
	store := newErrInjectingStore()
	store.connected = false
	enforcer := quota.NewEnforcer(manager, store, nil)

	_, err := enforcer.CheckAndReserve(context.Background(), quota.ScopeKey, "key_fc", "gpt", 100)
	if err == nil {
		t.Fatal("expected monthly-token fail-closed denial with Redis disconnected")
	}
	qerr, ok := err.(*quota.ExceededError)
	if !ok {
		t.Fatalf("expected ExceededError, got %T: %v", err, err)
	}
	if qerr.Limit != quota.LimitTotalTokensPerMonth {
		t.Fatalf("expected monthly token limit, got %s", qerr.Limit)
	}
}

func TestEnforcer_Concurrency_FailsClosed_WhenDisconnected(t *testing.T) {
	manager := policyWithLimits(map[quota.LimitType]int64{
		quota.LimitConcurrentRequests: 5,
	})
	store := newErrInjectingStore()
	store.connected = false
	enforcer := quota.NewEnforcer(manager, store, nil)

	if _, err := enforcer.CheckAndReserve(context.Background(), quota.ScopeKey, "key_fc", "gpt", 10); err == nil {
		t.Fatal("expected concurrency fail-closed denial with Redis disconnected (no per-instance fallback)")
	}
	// Nothing should have been counted in-memory.
	if store.concurrencyTotal() != 0 {
		t.Fatalf("expected no concurrency counted while disconnected, got %d", store.concurrencyTotal())
	}
}

// --- Connected Redis but counter op errors: fail closed --------------------

func TestEnforcer_FailsClosed_OnDailyAddError_Spend(t *testing.T) {
	manager := policyWithLimits(map[quota.LimitType]int64{
		quota.LimitConcurrentRequests: 5,
		quota.LimitDailySpendCents:    100,
	})
	store := newErrInjectingStore()
	store.failDailyAdd = true
	enforcer := quota.NewEnforcer(manager, store, nil)

	_, err := enforcer.CheckAndReserve(context.Background(), quota.ScopeKey, "key_fc", "gpt", 0, 500)
	if err == nil {
		t.Fatal("expected fail-closed denial when DailyAdd errors")
	}
	// Concurrency slot must be released (it was incremented before the spend op).
	if store.concurrencyTotal() != 0 {
		t.Fatalf("expected concurrency released after deny, got %d", store.concurrencyTotal())
	}
}

func TestEnforcer_FailsClosed_OnMonthlyAddError_RefundsDaily(t *testing.T) {
	manager := policyWithLimits(map[quota.LimitType]int64{
		quota.LimitConcurrentRequests: 5,
		quota.LimitDailySpendCents:    100,
		quota.LimitMonthlySpendCents:  100,
	})
	store := newErrInjectingStore()
	store.failMonthlyAdd = true // DailyAdd succeeds, MonthlyAdd errors.
	enforcer := quota.NewEnforcer(manager, store, nil)

	_, err := enforcer.CheckAndReserve(context.Background(), quota.ScopeKey, "key_fc", "gpt", 0, 500)
	if err == nil {
		t.Fatal("expected fail-closed denial when MonthlyAdd errors")
	}
	// The daily reservation taken before the monthly error must be refunded.
	if got := store.dailyWithSubstr("spend_millicents"); got != 0 {
		t.Fatalf("expected daily spend refunded to 0 after monthly error, got %d", got)
	}
	// Concurrency slot released.
	if store.concurrencyTotal() != 0 {
		t.Fatalf("expected concurrency released after deny, got %d", store.concurrencyTotal())
	}
}

func TestEnforcer_FailsClosed_OnRPMError(t *testing.T) {
	manager := policyWithLimits(map[quota.LimitType]int64{
		quota.LimitConcurrentRequests: 5,
		quota.LimitRequestsPerMinute:  10,
	})
	store := newErrInjectingStore()
	store.failSlidingWindow = true
	enforcer := quota.NewEnforcer(manager, store, nil)

	if _, err := enforcer.CheckAndReserve(context.Background(), quota.ScopeKey, "key_fc", "gpt", 0); err == nil {
		t.Fatal("expected fail-closed denial when SlidingWindowAdd (RPM) errors")
	}
	if store.concurrencyTotal() != 0 {
		t.Fatalf("expected concurrency released after deny, got %d", store.concurrencyTotal())
	}
}

func TestEnforcer_FailsClosed_OnTPMError(t *testing.T) {
	manager := policyWithLimits(map[quota.LimitType]int64{
		quota.LimitConcurrentRequests: 5,
		quota.LimitTokensPerMinute:    1000,
	})
	store := newErrInjectingStore()
	store.failSlidingWindow = true
	enforcer := quota.NewEnforcer(manager, store, nil)

	if _, err := enforcer.CheckAndReserve(context.Background(), quota.ScopeKey, "key_fc", "gpt", 50); err == nil {
		t.Fatal("expected fail-closed denial when SlidingWindowAdd (TPM) errors")
	}
	if store.concurrencyTotal() != 0 {
		t.Fatalf("expected concurrency released after deny, got %d", store.concurrencyTotal())
	}
}

func TestEnforcer_FailsClosed_OnDailyTokensAddError(t *testing.T) {
	manager := policyWithLimits(map[quota.LimitType]int64{
		quota.LimitConcurrentRequests: 5,
		quota.LimitTotalTokensPerDay:  500,
	})
	store := newErrInjectingStore()
	store.failDailyAdd = true
	enforcer := quota.NewEnforcer(manager, store, nil)

	if _, err := enforcer.CheckAndReserve(context.Background(), quota.ScopeKey, "key_fc", "gpt", 100); err == nil {
		t.Fatal("expected fail-closed denial when daily-token DailyAdd errors")
	}
	if store.concurrencyTotal() != 0 {
		t.Fatalf("expected concurrency released after deny, got %d", store.concurrencyTotal())
	}
}

func TestEnforcer_FailsClosed_OnMonthlyTokensAddError(t *testing.T) {
	manager := policyWithLimits(map[quota.LimitType]int64{
		quota.LimitConcurrentRequests:  5,
		quota.LimitTotalTokensPerMonth: 500,
	})
	store := newErrInjectingStore()
	store.failMonthlyAdd = true
	enforcer := quota.NewEnforcer(manager, store, nil)

	if _, err := enforcer.CheckAndReserve(context.Background(), quota.ScopeKey, "key_fc", "gpt", 100); err == nil {
		t.Fatal("expected fail-closed denial when monthly-token MonthlyAdd errors")
	}
	if store.concurrencyTotal() != 0 {
		t.Fatalf("expected concurrency released after deny, got %d", store.concurrencyTotal())
	}
}

// --- Sanctioned fail-open: NO limits configured, Redis down ----------------

func TestEnforcer_FailsOpen_NoLimitsConfigured_RedisDown(t *testing.T) {
	// A request with NO spend at stake (estimatedSpend == 0) and no explicit
	// rate/token/concurrency limits has nothing to enforce, so a Redis outage
	// must still ALLOW it. This pins the fail-open boundary.
	//
	// NB: under the strict spend policy, the DEFAULT monthly-spend cap is a
	// fail-closed trigger for any spend-bearing request — so the fail-open case
	// is specifically zero estimated spend (only the default concurrency cap,
	// which is availability-not-money, is skipped on a clean disconnect).
	manager := policyWithLimits(map[quota.LimitType]int64{})
	store := newErrInjectingStore()
	store.connected = false
	enforcer := quota.NewEnforcer(manager, store, nil)

	resv, err := enforcer.CheckAndReserve(context.Background(), quota.ScopeKey, "key_fc", "gpt", 100)
	if err != nil {
		t.Fatalf("expected fail-open allow with nothing at stake, got: %v", err)
	}
	if resv == nil {
		t.Fatal("expected a reservation on fail-open allow")
	}
}

// Sanity: with Redis connected and no error injection, behaviour is unchanged
// (allow under limits) — guards against the fail-closed paths leaking into the
// healthy path.
func TestEnforcer_HealthyRedis_AllowsUnderLimits(t *testing.T) {
	manager := policyWithLimits(map[quota.LimitType]int64{
		quota.LimitConcurrentRequests: 5,
		quota.LimitRequestsPerMinute:  10,
		quota.LimitTokensPerMinute:    1000,
		quota.LimitTotalTokensPerDay:  1000,
		quota.LimitDailySpendCents:    100,
		quota.LimitMonthlySpendCents:  100,
	})
	store := newErrInjectingStore()
	enforcer := quota.NewEnforcer(manager, store, nil)

	resv, err := enforcer.CheckAndReserve(context.Background(), quota.ScopeKey, "key_fc", "gpt", 50, 500)
	if err != nil {
		t.Fatalf("expected allow under limits with healthy Redis, got: %v", err)
	}
	if resv == nil {
		t.Fatal("expected a reservation when allowed")
	}
}
