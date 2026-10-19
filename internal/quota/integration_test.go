// Package quota_test provides comprehensive integration tests and a mock test
// harness for the quota system.
//
// The test harness includes:
//   - MockCounterStore — a full in-memory implementation of CounterStore with
//     weighted sliding-window semantics, spend tracking, and call inspection.
//   - QuotaClient — a lightweight wrapper simulating data-plane usage: it
//     resolves policies, reserves capacity, and reports usage per-request.
//   - Scenario tests — realistic multi-request workloads exercising all limit
//     types, concurrency, rollback, complete-with-correction, and degraded mode.
package quota_test

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/earlgrey/piproxy/internal/quota"
)

// ---------------------------------------------------------------------------
// MockCounterStore — a deterministic in-memory CounterStore for tests.
// ---------------------------------------------------------------------------

// windowEvent is a weighted event in a sliding window.
type windowEvent struct {
	ts     int64 // UnixMilli
	weight int64
}

// MockCounterStore implements quota.CounterStore with weighted sliding-window
// semantics, daily/monthly counters, and concurrency tracking. All state is
// in-memory; disconnected mode is toggled via SetConnected.
//
// Call tracking: every method records (method, key, amount) for assertions.
type MockCounterStore struct {
	mu        sync.Mutex
	connected bool

	windowEvents map[string][]windowEvent
	daily        map[string]int64
	monthly      map[string]int64
	concurrency  map[string]int64

	Calls []CallRecord
}

// CallRecord records a single counter-store operation.
type CallRecord struct {
	Order  int
	Method string
	Key    string
	Amount int64
}

func NewMockCounterStore() *MockCounterStore {
	return &MockCounterStore{
		connected:    true,
		windowEvents: make(map[string][]windowEvent),
		daily:        make(map[string]int64),
		monthly:      make(map[string]int64),
		concurrency:  make(map[string]int64),
	}
}

func (s *MockCounterStore) SetConnected(v bool) { s.mu.Lock(); defer s.mu.Unlock(); s.connected = v }
func (s *MockCounterStore) IsConnected() bool   { s.mu.Lock(); defer s.mu.Unlock(); return s.connected }

func (s *MockCounterStore) recordCall(method, key string, amount int64) {
	s.Calls = append(s.Calls, CallRecord{Order: len(s.Calls), Method: method, Key: key, Amount: amount})
}

// --- Concurrency ---

func (s *MockCounterStore) ConcurrencyIncr(_ context.Context, key string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordCall("ConcurrencyIncr", key, 1)
	s.concurrency[key]++
	return s.concurrency[key], nil
}

func (s *MockCounterStore) ConcurrencyDecr(_ context.Context, key string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordCall("ConcurrencyDecr", key, -1)
	s.concurrency[key]--
	if s.concurrency[key] <= 0 {
		delete(s.concurrency, key)
		return 0, nil
	}
	return s.concurrency[key], nil
}

// --- Sliding window (weighted) ---

func (s *MockCounterStore) SlidingWindowAdd(_ context.Context, key string, window time.Duration, amount int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordCall("SlidingWindowAdd", key, amount)

	now := time.Now().UnixMilli()
	cutoff := now - window.Milliseconds()

	s.windowEvents[key] = append(s.windowEvents[key], windowEvent{ts: now, weight: amount})

	// Prune expired and sum remaining weights.
	var kept []windowEvent
	var total int64
	for _, ev := range s.windowEvents[key] {
		if ev.ts >= cutoff {
			kept = append(kept, ev)
			total += ev.weight
		}
	}
	s.windowEvents[key] = kept
	return total, nil
}

func (s *MockCounterStore) WindowWeight(keySub string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, events := range s.windowEvents {
		if strings.Contains(key, keySub) {
			var total int64
			for _, ev := range events {
				total += ev.weight
			}
			return total
		}
	}
	return 0
}

// --- Daily / Monthly ---

func (s *MockCounterStore) DailyAdd(_ context.Context, key string, amount int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordCall("DailyAdd", key, amount)
	s.daily[key] += amount
	return s.daily[key], nil
}

func (s *MockCounterStore) MonthlyAdd(_ context.Context, key string, amount int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordCall("MonthlyAdd", key, amount)
	s.monthly[key] += amount
	return s.monthly[key], nil
}

// --- Test helpers ---

func (s *MockCounterStore) GetDaily(keySub string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, v := range s.daily {
		if strings.Contains(key, keySub) {
			return v
		}
	}
	return 0
}

func (s *MockCounterStore) GetMonthly(keySub string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, v := range s.monthly {
		if strings.Contains(key, keySub) {
			return v
		}
	}
	return 0
}

func (s *MockCounterStore) GetConcurrency(keySub string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, v := range s.concurrency {
		if strings.Contains(key, keySub) {
			return v
		}
	}
	return 0
}

func (s *MockCounterStore) CallsMatching(methodSub, keySub string) []CallRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []CallRecord
	for _, c := range s.Calls {
		if strings.Contains(c.Method, methodSub) && strings.Contains(c.Key, keySub) {
			out = append(out, c)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// QuotaClient — simulates how the data-plane calls the enforcer.
// ---------------------------------------------------------------------------

type QuotaClient struct {
	Enforcer *quota.Enforcer
	Store    *MockCounterStore
}

func NewQuotaClient() *QuotaClient {
	store := NewMockCounterStore()
	manager := quota.NewManager()
	enforcer := quota.NewEnforcer(manager, store, nil)
	return &QuotaClient{Enforcer: enforcer, Store: store}
}

// SetPolicies rebuilds the manager with the given policies.
func (c *QuotaClient) SetPolicies(policies ...*quota.Policy) {
	manager := quota.NewManager()
	manager.SetPolicies(policies)
	c.Enforcer = quota.NewEnforcer(manager, c.Store, nil)
}

func (c *QuotaClient) TryRequest(ctx context.Context, scopeType quota.ScopeType, scopeID, model string, estimatedTokens int64, estimatedSpendMillicents ...int64) (*quota.Reservation, error) {
	return c.Enforcer.CheckAndReserve(ctx, scopeType, scopeID, model, estimatedTokens, estimatedSpendMillicents...)
}

func (c *QuotaClient) CompleteRequest(ctx context.Context, resv *quota.Reservation, actualTokens int64, actualSpendMillicents ...int64) error {
	return c.Enforcer.Complete(ctx, resv, actualTokens, actualSpendMillicents...)
}

func (c *QuotaClient) RollbackRequest(ctx context.Context, resv *quota.Reservation) error {
	return c.Enforcer.Rollback(ctx, resv)
}

func (c *QuotaClient) MustReserve(t *testing.T, ctx context.Context, scopeType quota.ScopeType, scopeID, model string, estimatedTokens int64) *quota.Reservation {
	t.Helper()
	resv, err := c.TryRequest(ctx, scopeType, scopeID, model, estimatedTokens)
	if err != nil {
		t.Fatalf("expected reservation to succeed, got: %v", err)
	}
	return resv
}

func (c *QuotaClient) MustDeny(t *testing.T, ctx context.Context, scopeType quota.ScopeType, scopeID, model string, estimatedTokens int64, expectedLimit quota.LimitType) {
	t.Helper()
	_, err := c.TryRequest(ctx, scopeType, scopeID, model, estimatedTokens)
	if err == nil {
		t.Fatalf("expected quota denial for %s, but request succeeded", expectedLimit)
	}
	qerr, ok := err.(*quota.ExceededError)
	if !ok {
		t.Fatalf("expected ExceededError, got %T: %v", err, err)
	}
	if qerr.Limit != expectedLimit {
		t.Fatalf("expected limit %s, got %s", expectedLimit, qerr.Limit)
	}
}

// ---------------------------------------------------------------------------
// Scenario: Tenant with per-user RPM + daily token limits
// ---------------------------------------------------------------------------

func TestScenario_TenantUserRPMAndDailyTokens(t *testing.T) {
	ctx := context.Background()
	c := NewQuotaClient()

	c.SetPolicies(
		&quota.Policy{
			ID: "pol_user_1", TenantID: "tn_acme", ScopeType: quota.ScopeUser,
			ScopeID: "usr_alice", Priority: 1, Enabled: true,
			Limits: map[quota.LimitType]int64{
				quota.LimitRequestsPerMinute:  5,
				quota.LimitTotalTokensPerDay:  500,
				quota.LimitConcurrentRequests: 10,
			},
		},
	)

	// Alice fires 5 requests — all should succeed.
	reservations := make([]*quota.Reservation, 0, 5)
	for i := 0; i < 5; i++ {
		resv := c.MustReserve(t, ctx, quota.ScopeUser, "usr_alice", "gpt-4", 100)
		reservations = append(reservations, resv)
	}

	// 6th request should be denied by RPM.
	c.MustDeny(t, ctx, quota.ScopeUser, "usr_alice", "gpt-4", 100, quota.LimitRequestsPerMinute)

	// Complete all 5 → total tokens = 500.
	for _, resv := range reservations {
		if err := c.CompleteRequest(ctx, resv, 100); err != nil {
			t.Fatalf("complete failed: %v", err)
		}
	}

	dailyTokens := c.Store.GetDaily("tokens")
	if dailyTokens != 500 {
		t.Errorf("expected 500 daily tokens, got %d", dailyTokens)
	}
}

// ---------------------------------------------------------------------------
// Scenario: Spend limits with over/under reconciliation
// ---------------------------------------------------------------------------

func TestScenario_SpendReconciliation(t *testing.T) {
	ctx := context.Background()
	c := NewQuotaClient()

	// 1 cent = 1000 millicents. Set limit to 1 cent.
	c.SetPolicies(
		&quota.Policy{
			ID: "pol_spend", TenantID: "tn_acme", ScopeType: quota.ScopeKey,
			ScopeID: "*", Priority: 1, Enabled: true,
			Limits: map[quota.LimitType]int64{
				quota.LimitConcurrentRequests: 10,
				quota.LimitDailySpendCents:    1, // 1000 millicents
				quota.LimitMonthlySpendCents:  100,
			},
		},
	)

	// Estimate 700 millicents ($0.70).
	resv1, err := c.TryRequest(ctx, quota.ScopeKey, "key_alpha", "gpt-4", 10, 700)
	if err != nil {
		t.Fatalf("first reservation failed: %v", err)
	}

	// Complete with actual 900 millicents ($0.90) — adjust +200.
	if err := c.CompleteRequest(ctx, resv1, 10, 900); err != nil {
		t.Fatalf("complete failed: %v", err)
	}

	dailySpend := c.Store.GetDaily("spend_millicents")
	if dailySpend != 900 {
		t.Errorf("expected 900 millicents after reconcile, got %d", dailySpend)
	}

	// Next request estimates 200 millicents → total 1100 > 1000 limit.
	_, err2 := c.TryRequest(ctx, quota.ScopeKey, "key_alpha", "gpt-4", 10, 200)
	if err2 == nil {
		t.Fatal("expected daily spend denial")
	}
	qerr, ok := err2.(*quota.ExceededError)
	if !ok || qerr.Limit != quota.LimitDailySpendCents {
		t.Fatalf("expected daily spend limit, got %v", err2)
	}

	// Spend refunded after denial.
	dailySpend = c.Store.GetDaily("spend_millicents")
	if dailySpend != 900 {
		t.Errorf("expected refund to 900, got %d", dailySpend)
	}
}

// ---------------------------------------------------------------------------
// Scenario: Concurrent request limit
// ---------------------------------------------------------------------------

func TestScenario_ConcurrencyLimit(t *testing.T) {
	ctx := context.Background()
	c := NewQuotaClient()

	c.SetPolicies(
		&quota.Policy{
			ID: "pol_conc", TenantID: "tn_acme", ScopeType: quota.ScopeTenant,
			ScopeID: "*", Priority: 0, Enabled: true,
			Limits: map[quota.LimitType]int64{
				quota.LimitConcurrentRequests: 2,
			},
		},
	)

	resv1 := c.MustReserve(t, ctx, quota.ScopeTenant, "tn_acme", "gpt-4", 10)
	resv2 := c.MustReserve(t, ctx, quota.ScopeTenant, "tn_acme", "gpt-4", 10)
	c.MustDeny(t, ctx, quota.ScopeTenant, "tn_acme", "gpt-4", 10, quota.LimitConcurrentRequests)

	if err := c.CompleteRequest(ctx, resv1, 10); err != nil {
		t.Fatalf("complete failed: %v", err)
	}
	_ = c.MustReserve(t, ctx, quota.ScopeTenant, "tn_acme", "gpt-4", 10)

	if err := c.CompleteRequest(ctx, resv2, 10); err != nil {
		t.Fatalf("complete failed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Scenario: Most-restrictive-wins across multiple policies
// ---------------------------------------------------------------------------

func TestScenario_MostRestrictiveWins(t *testing.T) {
	ctx := context.Background()
	c := NewQuotaClient()

	// Tenant-wide generous, user-specific restrictive.
	c.SetPolicies(
		&quota.Policy{
			ID: "pol_tenant", TenantID: "tn_acme", ScopeType: quota.ScopeTenant,
			ScopeID: "*", Priority: 0, Enabled: true,
			Limits: map[quota.LimitType]int64{
				quota.LimitRequestsPerMinute:  100,
				quota.LimitTokensPerMinute:    50000,
				quota.LimitConcurrentRequests: 100,
			},
		},
		&quota.Policy{
			ID: "pol_user", TenantID: "tn_acme", ScopeType: quota.ScopeUser,
			ScopeID: "usr_bob", Priority: 10, Enabled: true,
			Limits: map[quota.LimitType]int64{
				quota.LimitRequestsPerMinute:  5,
				quota.LimitTokensPerMinute:    10000,
				quota.LimitConcurrentRequests: 100,
			},
		},
	)

	// Bob hits his user-level RPM (5).
	for i := 0; i < 5; i++ {
		_ = c.MustReserve(t, ctx, quota.ScopeUser, "usr_bob", "gpt-4", 100)
	}
	c.MustDeny(t, ctx, quota.ScopeUser, "usr_bob", "gpt-4", 100, quota.LimitRequestsPerMinute)

	// Another user gets tenant-wide limits.
	_ = c.MustReserve(t, ctx, quota.ScopeUser, "usr_carol", "gpt-4", 100)
}

// ---------------------------------------------------------------------------
// Scenario: Max input token guard (no counter mutation)
// ---------------------------------------------------------------------------

func TestScenario_MaxInputTokensNoCounterMutation(t *testing.T) {
	ctx := context.Background()
	c := NewQuotaClient()

	c.SetPolicies(
		&quota.Policy{
			ID: "pol_input", TenantID: "tn_acme", ScopeType: quota.ScopeKey,
			ScopeID: "*", Priority: 0, Enabled: true,
			Limits: map[quota.LimitType]int64{
				quota.LimitMaxInputTokens:     1000,
				quota.LimitRequestsPerMinute:  100,
				quota.LimitConcurrentRequests: 100,
			},
		},
	)

	callCountBefore := len(c.Store.Calls)

	c.MustDeny(t, ctx, quota.ScopeKey, "key_x", "gpt-4", 2000, quota.LimitMaxInputTokens)

	if len(c.Store.Calls) != callCountBefore {
		t.Errorf("expected no counter mutations, got %d new calls", len(c.Store.Calls)-callCountBefore)
	}

	resv, err := c.TryRequest(ctx, quota.ScopeKey, "key_x", "gpt-4", 500)
	if err != nil {
		t.Fatalf("expected reservation within input limit: %v", err)
	}
	if err := c.CompleteRequest(ctx, resv, 500); err != nil {
		t.Fatalf("complete failed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Scenario: Disabled policy does not apply
// ---------------------------------------------------------------------------

func TestScenario_DisabledPolicyIgnored(t *testing.T) {
	ctx := context.Background()
	c := NewQuotaClient()

	c.SetPolicies(
		&quota.Policy{
			ID: "pol_disabled", TenantID: "tn_acme", ScopeType: quota.ScopeKey,
			ScopeID: "*", Priority: 0, Enabled: false,
			Limits: map[quota.LimitType]int64{
				quota.LimitRequestsPerMinute:  1,
				quota.LimitConcurrentRequests: 0,
			},
		},
	)

	resv := c.MustReserve(t, ctx, quota.ScopeKey, "key_y", "gpt-4", 50)
	if err := c.CompleteRequest(ctx, resv, 50); err != nil {
		t.Fatalf("complete failed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Scenario: Disconnected Redis — FAIL CLOSED on enforced dimensions
//
// H1 fix: a request that hits a configured concurrency / RPM / token-total
// limit while Redis is disconnected must be DENIED, not degraded-open. Allowing
// it would make concurrency per-instance (effective N*K cluster limit) and the
// rate/token limits unenforced. This is the deliberate behavioural contract
// change from the previous "degraded but functional" expectation.
// ---------------------------------------------------------------------------

func TestScenario_DisconnectedRedis(t *testing.T) {
	ctx := context.Background()
	c := NewQuotaClient()

	c.SetPolicies(
		&quota.Policy{
			ID: "pol_deg", TenantID: "tn_acme", ScopeType: quota.ScopeKey,
			ScopeID: "*", Priority: 0, Enabled: true,
			Limits: map[quota.LimitType]int64{
				quota.LimitConcurrentRequests: 2,
				quota.LimitRequestsPerMinute:  5,
				quota.LimitTotalTokensPerDay:  500,
			},
		},
	)

	c.Store.SetConnected(false)

	// Concurrency is checked first; with Redis down it fails closed and the
	// enforcer returns a (non-ExceededError) error rather than reserving via a
	// per-instance in-memory counter.
	if _, err := c.TryRequest(ctx, quota.ScopeKey, "key_z", "gpt-4", 100); err == nil {
		t.Fatal("expected concurrency check to fail closed with Redis disconnected, but request succeeded")
	}
}

// ---------------------------------------------------------------------------
// Scenario: Rollback refunds all reservations
// ---------------------------------------------------------------------------

func TestScenario_RollbackFullRefund(t *testing.T) {
	ctx := context.Background()
	c := NewQuotaClient()

	c.SetPolicies(
		&quota.Policy{
			ID: "pol_rb", TenantID: "tn_acme", ScopeType: quota.ScopeKey,
			ScopeID: "*", Priority: 0, Enabled: true,
			Limits: map[quota.LimitType]int64{
				quota.LimitConcurrentRequests: 10,
				quota.LimitDailySpendCents:    100,
				quota.LimitMonthlySpendCents:  200,
			},
		},
	)

	resv, err := c.TryRequest(ctx, quota.ScopeKey, "key_rb", "gpt-4", 10, 800)
	if err != nil {
		t.Fatalf("reservation failed: %v", err)
	}

	if err := c.RollbackRequest(ctx, resv); err != nil {
		t.Fatalf("rollback failed: %v", err)
	}

	dailySpend := c.Store.GetDaily("spend_millicents")
	monthlySpend := c.Store.GetMonthly("spend_millicents")
	if dailySpend != 0 {
		t.Errorf("expected daily spend 0 after rollback, got %d", dailySpend)
	}
	if monthlySpend != 0 {
		t.Errorf("expected monthly spend 0 after rollback, got %d", monthlySpend)
	}
	if conc := c.Store.GetConcurrency("concurrency"); conc != 0 {
		t.Errorf("expected concurrency 0 after rollback, got %d", conc)
	}
}

// ---------------------------------------------------------------------------
// Scenario: Double-complete is safe
// ---------------------------------------------------------------------------

func TestScenario_DoubleCompleteSafe(t *testing.T) {
	ctx := context.Background()
	c := NewQuotaClient()

	c.SetPolicies(
		&quota.Policy{
			ID: "pol_dc", TenantID: "tn_acme", ScopeType: quota.ScopeKey,
			ScopeID: "*", Priority: 0, Enabled: true,
			Limits: map[quota.LimitType]int64{
				quota.LimitConcurrentRequests: 2,
			},
		},
	)

	resv := c.MustReserve(t, ctx, quota.ScopeKey, "key_dc", "gpt-4", 10)
	if err := c.CompleteRequest(ctx, resv, 10); err != nil {
		t.Fatalf("first complete: %v", err)
	}
	if err := c.CompleteRequest(ctx, resv, 10); err != nil {
		t.Fatalf("second complete: %v", err)
	}
	if conc := c.Store.GetConcurrency("concurrency"); conc != 0 {
		t.Errorf("expected concurrency 0, got %d", conc)
	}
}

// ---------------------------------------------------------------------------
// Scenario: Concurrent reservations — serialise via mutex to avoid races
// ---------------------------------------------------------------------------

func TestScenario_ConcurrentReservations(t *testing.T) {
	ctx := context.Background()
	c := NewQuotaClient()

	const N = 50

	c.SetPolicies(
		&quota.Policy{
			ID: "pol_many", TenantID: "tn_acme", ScopeType: quota.ScopeKey,
			ScopeID: "*", Priority: 0, Enabled: true,
			Limits: map[quota.LimitType]int64{
				quota.LimitConcurrentRequests: int64(N),
				quota.LimitRequestsPerMinute:  int64(N * 2),
				quota.LimitTokensPerMinute:    int64(N * 100 * 2),
			},
		},
	)

	var wg sync.WaitGroup
	errs := make(chan error, N)

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resv, err := c.TryRequest(ctx, quota.ScopeKey, "key_many", "gpt-4", 100)
			if err != nil {
				errs <- fmt.Errorf("reserve %d: %w", i, err)
				return
			}
			if err := c.CompleteRequest(ctx, resv, 100); err != nil {
				errs <- fmt.Errorf("complete %d: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	failures := 0
	for err := range errs {
		if err != nil {
			t.Logf("concurrent error: %v", err)
			failures++
		}
	}
	if failures > 0 {
		t.Errorf("expected 0 failures, got %d", failures)
	}
	if conc := c.Store.GetConcurrency("concurrency"); conc != 0 {
		t.Errorf("expected concurrency 0 after all complete, got %d", conc)
	}
}

// ---------------------------------------------------------------------------
// Scenario: Mixed scope policies (tenant, user, key)
// ---------------------------------------------------------------------------

func TestScenario_MultiScopePolicies(t *testing.T) {
	ctx := context.Background()
	c := NewQuotaClient()

	c.SetPolicies(
		&quota.Policy{
			ID: "pol_tenant", TenantID: "tn_acme", ScopeType: quota.ScopeTenant,
			ScopeID: "tn_acme", Priority: 0, Enabled: true,
			Limits: map[quota.LimitType]int64{
				quota.LimitRequestsPerMinute:  100,
				quota.LimitDailySpendCents:    5000,
				quota.LimitConcurrentRequests: 100,
			},
		},
		&quota.Policy{
			ID: "pol_user", TenantID: "tn_acme", ScopeType: quota.ScopeUser,
			ScopeID: "usr_eve", Priority: 10, Enabled: true,
			Limits: map[quota.LimitType]int64{
				quota.LimitRequestsPerMinute:  10,
				quota.LimitConcurrentRequests: 100,
			},
		},
		&quota.Policy{
			ID: "pol_key", TenantID: "tn_acme", ScopeType: quota.ScopeKey,
			ScopeID: "key_eve_1", Priority: 20, Enabled: true,
			Limits: map[quota.LimitType]int64{
				quota.LimitRequestsPerMinute:  5,
				quota.LimitDailySpendCents:    500,
				quota.LimitConcurrentRequests: 100,
			},
		},
	)

	// Key-level RPM (5) is most restrictive. Enforcer checks by scopeType+scopeID.
	// The enforcer only checks the scope we pass, not all scopes simultaneously.
	// When we pass ScopeKey + "key_eve_1", only key-level policy applies.
	for i := 0; i < 5; i++ {
		_ = c.MustReserve(t, ctx, quota.ScopeKey, "key_eve_1", "gpt-4", 10)
	}
	c.MustDeny(t, ctx, quota.ScopeKey, "key_eve_1", "gpt-4", 10, quota.LimitRequestsPerMinute)

	// A different scope (user-level) gets user-level RPM (10).
	for i := 0; i < 10; i++ {
		_ = c.MustReserve(t, ctx, quota.ScopeUser, "usr_eve", "gpt-4", 10)
	}
	c.MustDeny(t, ctx, quota.ScopeUser, "usr_eve", "gpt-4", 10, quota.LimitRequestsPerMinute)
}

// ---------------------------------------------------------------------------
// Scenario: TPM limits with weighted estimation
// ---------------------------------------------------------------------------

func TestScenario_TPMWithLargeEstimate(t *testing.T) {
	ctx := context.Background()
	c := NewQuotaClient()

	c.SetPolicies(
		&quota.Policy{
			ID: "pol_tpm", TenantID: "tn_acme", ScopeType: quota.ScopeKey,
			ScopeID: "*", Priority: 0, Enabled: true,
			Limits: map[quota.LimitType]int64{
				quota.LimitTokensPerMinute:    1000,
				quota.LimitConcurrentRequests: 10,
			},
		},
	)

	// 600 tokens → ok.
	resv := c.MustReserve(t, ctx, quota.ScopeKey, "key_tpm", "gpt-4", 600)
	if err := c.CompleteRequest(ctx, resv, 600); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// 500 tokens → 1100 > 1000, denied.
	// Note: the mock sliding window expires old events, but both events are "now"
	// so they sum to 1100.
	c.MustDeny(t, ctx, quota.ScopeKey, "key_tpm", "gpt-4", 500, quota.LimitTokensPerMinute)
}

// ---------------------------------------------------------------------------
// Scenario: Monthly token limits
// ---------------------------------------------------------------------------

func TestScenario_MonthlyTokenLimit(t *testing.T) {
	ctx := context.Background()
	c := NewQuotaClient()

	c.SetPolicies(
		&quota.Policy{
			ID: "pol_monthly", TenantID: "tn_acme", ScopeType: quota.ScopeKey,
			ScopeID: "*", Priority: 0, Enabled: true,
			Limits: map[quota.LimitType]int64{
				quota.LimitConcurrentRequests:  100,
				quota.LimitRequestsPerMinute:   100,
				quota.LimitTokensPerMinute:     100000,
				quota.LimitTotalTokensPerMonth: 1000,
			},
		},
	)

	for i := 0; i < 10; i++ {
		resv := c.MustReserve(t, ctx, quota.ScopeKey, "key_mo", "gpt-4", 100)
		if err := c.CompleteRequest(ctx, resv, 100); err != nil {
			t.Fatalf("complete %d: %v", i, err)
		}
	}

	c.MustDeny(t, ctx, quota.ScopeKey, "key_mo", "gpt-4", 100, quota.LimitTotalTokensPerMonth)
}

// ---------------------------------------------------------------------------
// Scenario: ExceededError formatting
// ---------------------------------------------------------------------------

func TestScenario_ExceededErrorFormatting(t *testing.T) {
	err := &quota.ExceededError{
		Scope:   quota.ScopeKey,
		Limit:   quota.LimitRequestsPerMinute,
		Message: "RPM limit 5 exceeded (6 this minute)",
	}
	msg := err.Error()
	if !strings.Contains(msg, "quota exceeded") {
		t.Errorf("expected 'quota exceeded', got: %s", msg)
	}
	if !strings.Contains(msg, string(quota.ScopeKey)) {
		t.Errorf("expected scope, got: %s", msg)
	}
}

// ---------------------------------------------------------------------------
// Scenario: Zero estimated tokens skips token limits
// ---------------------------------------------------------------------------

func TestScenario_ZeroTokensSkipsLimits(t *testing.T) {
	ctx := context.Background()
	c := NewQuotaClient()

	c.SetPolicies(
		&quota.Policy{
			ID: "pol_zero", TenantID: "tn_acme", ScopeType: quota.ScopeKey,
			ScopeID: "*", Priority: 0, Enabled: true,
			Limits: map[quota.LimitType]int64{
				quota.LimitConcurrentRequests: 10,
				quota.LimitTokensPerMinute:    1,
				quota.LimitTotalTokensPerDay:  1,
				quota.LimitMaxInputTokens:     100,
			},
		},
	)

	resv, err := c.TryRequest(ctx, quota.ScopeKey, "key_zero", "gpt-4", 0)
	if err != nil {
		t.Fatalf("zero-token reservation should succeed: %v", err)
	}
	if err := c.CompleteRequest(ctx, resv, 0); err != nil {
		t.Fatalf("complete: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Scenario: Nil enforcer safety
// ---------------------------------------------------------------------------

func TestScenario_NilEnforcerSafe(t *testing.T) {
	var enforcer *quota.Enforcer
	limit := enforcer.EffectiveLimit(quota.ScopeKey, "any", quota.LimitRequestsPerMinute)
	if limit != 0 {
		t.Errorf("nil enforcer should return 0, got %d", limit)
	}
}

// ---------------------------------------------------------------------------
// Scenario: SpendCounterScale ensures millicent precision
// ---------------------------------------------------------------------------

func TestScenario_SpendCounterScale(t *testing.T) {
	if quota.SpendCounterScale != 1000 {
		t.Fatalf("SpendCounterScale must be 1000, got %d", quota.SpendCounterScale)
	}

	ctx := context.Background()
	c := NewQuotaClient()

	c.SetPolicies(
		&quota.Policy{
			ID: "pol_scale", TenantID: "tn_acme", ScopeType: quota.ScopeKey,
			ScopeID: "*", Priority: 0, Enabled: true,
			Limits: map[quota.LimitType]int64{
				quota.LimitConcurrentRequests: 10,
				quota.LimitDailySpendCents:    1, // 1000 millicents
			},
		},
	)

	// 999 millicents < 1 cent — ok.
	resv, err := c.TryRequest(ctx, quota.ScopeKey, "key_scale", "gpt-4", 10, 999)
	if err != nil {
		t.Fatalf("999 millicents should be under limit: %v", err)
	}
	if err := c.CompleteRequest(ctx, resv, 10, 999); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// 2 more millicents → 1001 > 1000 → denied.
	_, err3 := c.TryRequest(ctx, quota.ScopeKey, "key_scale", "gpt-4", 10, 2)
	if err3 == nil {
		t.Fatal("expected daily spend denial at 1001 millicents")
	}
	qerr3, ok3 := err3.(*quota.ExceededError)
	if !ok3 || qerr3.Limit != quota.LimitDailySpendCents {
		t.Fatalf("expected daily spend limit, got %v", err3)
	}
}

// ---------------------------------------------------------------------------
// Scenario: Wildcard scope matches any specific ID
// ---------------------------------------------------------------------------

func TestScenario_WildcardScopeMatchesAnyID(t *testing.T) {
	m := quota.NewManager()
	m.SetPolicies([]*quota.Policy{
		{
			ID: "pol_wild", TenantID: "tn_acme", ScopeType: quota.ScopeUser,
			ScopeID: "*", Priority: 0, Enabled: true,
			Limits: map[quota.LimitType]int64{
				quota.LimitRequestsPerMinute: 7,
			},
		},
	})

	for _, id := range []string{"usr_a", "usr_b", "usr_anything", ""} {
		limit := m.GetEffectiveLimit(quota.ScopeUser, id, quota.LimitRequestsPerMinute)
		if limit != 7 {
			t.Errorf("wildcard should match %q, got limit=%d", id, limit)
		}
	}
}

// ---------------------------------------------------------------------------
// TokenBucket tests
// ---------------------------------------------------------------------------

func TestTokenBucket_Basic(t *testing.T) {
	tb := quota.NewTokenBucket(10, 2)
	if !tb.Allow(10) {
		t.Error("expected allow for 10 tokens")
	}
	if tb.Allow(1) {
		t.Error("expected deny when empty")
	}
	time.Sleep(600 * time.Millisecond)
	if !tb.Allow(1) {
		t.Error("expected allow after refill")
	}
	if tb.Allow(1) {
		t.Error("expected only ~1 token refilled")
	}
}

func TestTokenBucket_NeverExceedsMax(t *testing.T) {
	tb := quota.NewTokenBucket(5, 100)
	time.Sleep(200 * time.Millisecond)
	if !tb.Allow(5) {
		t.Error("expected allow (max)")
	}
	if tb.Allow(1) {
		t.Error("bucket should not exceed max")
	}
}

// ---------------------------------------------------------------------------
// InLimits tests
// ---------------------------------------------------------------------------

func TestInLimits_GlobMatching(t *testing.T) {
	tests := []struct {
		model    string
		patterns []string
		want     bool
	}{
		{"gpt-4", []string{"gpt-*"}, true},
		{"gpt-4-turbo", []string{"gpt-*"}, true},
		{"claude-opus-4", []string{"gpt-*"}, false},
		{"gpt-4", []string{"gpt-4"}, true},
		{"gpt-4-turbo", []string{"gpt-4"}, false},
		{"claude-sonnet-4", []string{"claude-*"}, true},
		{"gpt-3.5", []string{"gpt-4*", "claude-*"}, false},
		// "*" pattern: suffix "*" → prefix "" → HasPrefix always true.
		{"gemini-pro", []string{"*"}, true},
		{"anything", []string{"*"}, true},
	}

	for _, tt := range tests {
		got := quota.InLimits(tt.model, tt.patterns)
		if got != tt.want {
			t.Errorf("InLimits(%q, %v) = %v, want %v", tt.model, tt.patterns, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// EstimateTokens / EstimateMessagesTokens
// ---------------------------------------------------------------------------

func TestEstimateTokens_ApproximatelyCorrect(t *testing.T) {
	tok := quota.EstimateTokens("hello world, this is a test")
	if tok < 5 || tok > 10 {
		t.Errorf("expected 5-10 tokens, got %d", tok)
	}
	tok = quota.EstimateTokens("")
	if tok != 0 {
		t.Errorf("expected 0 for empty, got %d", tok)
	}
}

func TestEstimateMessagesTokens_IncludesOverhead(t *testing.T) {
	total := quota.EstimateMessagesTokens([]string{"Hello", "How are you?"})
	if total < 20 || total > 30 {
		t.Errorf("expected ~24 tokens, got %d", total)
	}
}

// ---------------------------------------------------------------------------
// ValidatePolicy edge cases
// ---------------------------------------------------------------------------

func TestValidatePolicy_AllScopeTypes(t *testing.T) {
	for _, s := range []quota.ScopeType{
		quota.ScopeTenant, quota.ScopeTeam, quota.ScopeUser,
		quota.ScopeKey, quota.ScopeProvider, quota.ScopeModel,
	} {
		p := &quota.Policy{ID: "p", TenantID: "t", ScopeType: s, ScopeID: "x"}
		if err := quota.ValidatePolicy(p); err != nil {
			t.Errorf("scope %s should be valid: %v", s, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Policy priority does NOT affect effective limit (most restrictive wins)
// ---------------------------------------------------------------------------

func TestPolicyPriority_DoesNotMatter(t *testing.T) {
	m := quota.NewManager()
	m.SetPolicies([]*quota.Policy{
		{ID: "low", TenantID: "tn", ScopeType: quota.ScopeKey, ScopeID: "key_x", Priority: 100, Enabled: true,
			Limits: map[quota.LimitType]int64{quota.LimitRequestsPerMinute: 5}},
		{ID: "high", TenantID: "tn", ScopeType: quota.ScopeKey, ScopeID: "key_x", Priority: 0, Enabled: true,
			Limits: map[quota.LimitType]int64{quota.LimitRequestsPerMinute: 10}},
	})
	if limit := m.GetEffectiveLimit(quota.ScopeKey, "key_x", quota.LimitRequestsPerMinute); limit != 5 {
		t.Errorf("expected 5 (most restrictive), got %d", limit)
	}
}

// ---------------------------------------------------------------------------
// Default limits are sensible
// ---------------------------------------------------------------------------

func TestDefaultLimits_AreSensible(t *testing.T) {
	dl := quota.DefaultLimits()
	if dl[quota.LimitMonthlySpendCents] <= 0 {
		t.Error("monthly spend default must be > 0")
	}
	if dl[quota.LimitMonthlySpendCents] > 1_000_000 {
		t.Error("monthly spend default unreasonably high")
	}
	if dl[quota.LimitConcurrentRequests] <= 0 {
		t.Error("concurrency default must be > 0")
	}
	if dl[quota.LimitMaxBodyBytes] < 1*1024*1024 {
		t.Error("max body bytes default too low")
	}
}

// Compile guards
var _ = sort.Ints
var _ = math.Abs
