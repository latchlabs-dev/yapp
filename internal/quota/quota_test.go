package quota_test

import (
	"testing"

	"github.com/earlgrey/piproxy/internal/quota"
)

func TestNewManager(t *testing.T) {
	m := quota.NewManager()
	if m == nil {
		t.Fatal("expected non-nil manager")
	}
}

func TestDefaultLimits(t *testing.T) {
	limits := quota.DefaultLimits()
	if len(limits) == 0 {
		t.Error("expected non-empty default limits")
	}
	if limits[quota.LimitMonthlySpendCents] != 50000 {
		t.Errorf("expected 50000 monthly spend cents, got %d", limits[quota.LimitMonthlySpendCents])
	}
	if limits[quota.LimitRequestsPerMinute] != 6000 {
		t.Errorf("expected default RPM safety-net 6000, got %d", limits[quota.LimitRequestsPerMinute])
	}
	if limits[quota.LimitDailySpendCents] != 10000 {
		t.Errorf("expected default daily spend 10000 cents, got %d", limits[quota.LimitDailySpendCents])
	}
	if limits[quota.LimitTokensPerMinute] != 10_000_000 {
		t.Errorf("expected default TPM 10_000_000, got %d", limits[quota.LimitTokensPerMinute])
	}
}

func TestAggregateScopeConcurrencyDefaults(t *testing.T) {
	m := quota.NewManager()

	// A bare API key keeps the flat default of 64 concurrent requests.
	if got := m.GetEffectiveLimit(quota.ScopeKey, "key_1", quota.LimitConcurrentRequests); got != 64 {
		t.Errorf("api_key concurrency default: expected 64, got %d", got)
	}
	// A user typically drives several concurrent sessions, so gets a higher cap.
	if got := m.GetEffectiveLimit(quota.ScopeUser, "usr_1", quota.LimitConcurrentRequests); got != 128 {
		t.Errorf("user concurrency default: expected 128, got %d", got)
	}

	// Aggregate scopes fan many actors together, so they must NOT inherit the
	// per-actor default — that was the tenant-wide 429 bug. They get a much higher
	// default ceiling.
	if got := m.GetEffectiveLimit(quota.ScopeTenant, "ten_1", quota.LimitConcurrentRequests); got != 2048 {
		t.Errorf("tenant concurrency default: expected 2048, got %d", got)
	}
	if got := m.GetEffectiveLimit(quota.ScopeTeam, "team_1", quota.LimitConcurrentRequests); got != 512 {
		t.Errorf("team concurrency default: expected 512, got %d", got)
	}

	// The override only affects concurrency; other tenant defaults still come
	// from the flat table.
	if got := m.GetEffectiveLimit(quota.ScopeTenant, "ten_1", quota.LimitRequestsPerMinute); got != 6000 {
		t.Errorf("tenant RPM default: expected flat 6000, got %d", got)
	}
}

func TestExplicitPolicyOverridesScopeDefault(t *testing.T) {
	m := quota.NewManager()
	m.SetPolicies([]*quota.Policy{
		{
			ID:        "pol_tenant_conc",
			TenantID:  "ten_1",
			ScopeType: quota.ScopeTenant,
			ScopeID:   "ten_1",
			Priority:  1,
			Limits:    map[quota.LimitType]int64{quota.LimitConcurrentRequests: 10},
			Enabled:   true,
		},
	})

	// An explicit tenant policy wins over the higher scope default.
	if got := m.GetEffectiveLimit(quota.ScopeTenant, "ten_1", quota.LimitConcurrentRequests); got != 10 {
		t.Errorf("explicit tenant concurrency: expected 10, got %d", got)
	}
	// A different tenant with no explicit policy still gets the scope default.
	if got := m.GetEffectiveLimit(quota.ScopeTenant, "ten_2", quota.LimitConcurrentRequests); got != 2048 {
		t.Errorf("unconfigured tenant concurrency: expected default 2048, got %d", got)
	}
}

func TestGetEffectiveLimit(t *testing.T) {
	m := quota.NewManager()
	m.SetPolicies([]*quota.Policy{
		{
			ID:        "pol_1",
			TenantID:  "ten_123",
			ScopeType: quota.ScopeUser,
			ScopeID:   "usr_123",
			Priority:  1,
			Limits:    map[quota.LimitType]int64{quota.LimitRequestsPerMinute: 30},
			Enabled:   true,
		},
	})

	// Should use the policy value.
	limit := m.GetEffectiveLimit(quota.ScopeUser, "usr_123", quota.LimitRequestsPerMinute)
	if limit != 30 {
		t.Errorf("expected 30, got %d", limit)
	}

	// Should fall back to the user scope default for a non-configured limit.
	limit = m.GetEffectiveLimit(quota.ScopeUser, "usr_123", quota.LimitConcurrentRequests)
	if limit != 128 {
		t.Errorf("expected user default 128, got %d", limit)
	}

	// Should use the default RPM safety-net for an unmatching scope.
	limit = m.GetEffectiveLimit(quota.ScopeUser, "nonexistent", quota.LimitRequestsPerMinute)
	if limit != 6000 {
		t.Errorf("expected default RPM safety-net 6000, got %d", limit)
	}
}

func TestGetEffectiveLimitWildcard(t *testing.T) {
	m := quota.NewManager()
	m.SetPolicies([]*quota.Policy{
		{
			ID:        "pol_wild",
			TenantID:  "ten_123",
			ScopeType: quota.ScopeUser,
			ScopeID:   "*",
			Priority:  0,
			Limits:    map[quota.LimitType]int64{quota.LimitRequestsPerMinute: 50},
			Enabled:   true,
		},
	})

	limit := m.GetEffectiveLimit(quota.ScopeUser, "any_user", quota.LimitRequestsPerMinute)
	if limit != 50 {
		t.Errorf("expected 50 from wildcard, got %d", limit)
	}
}

func TestDisabledPolicy(t *testing.T) {
	m := quota.NewManager()
	m.SetPolicies([]*quota.Policy{
		{
			ID:        "pol_disabled",
			TenantID:  "ten_123",
			ScopeType: quota.ScopeUser,
			ScopeID:   "usr_123",
			Limits:    map[quota.LimitType]int64{quota.LimitRequestsPerMinute: 10},
			Enabled:   false,
		},
	})

	// A disabled policy is ignored, so the RPM default safety-net applies.
	limit := m.GetEffectiveLimit(quota.ScopeUser, "usr_123", quota.LimitRequestsPerMinute)
	if limit != 6000 {
		t.Errorf("expected default RPM safety-net 6000 for disabled policy, got %d", limit)
	}
}

func TestUserSpendCapsAreIndependentOfTenantBudget(t *testing.T) {
	m := quota.NewManager()
	m.SetPolicies([]*quota.Policy{
		{ID: "tenant", TenantID: "t", ScopeType: quota.ScopeTenant, ScopeID: "t", Enabled: true, Limits: map[quota.LimitType]int64{quota.LimitDailySpendCents: 20000, quota.LimitMonthlySpendCents: 55000}},
		{ID: "user", TenantID: "t", ScopeType: quota.ScopeUser, ScopeID: "u", Enabled: true, Limits: map[quota.LimitType]int64{quota.LimitMonthlySpendCents: 25000}},
		{ID: "daily", TenantID: "t", ScopeType: quota.ScopeUser, ScopeID: "capped", Enabled: true, Limits: map[quota.LimitType]int64{quota.LimitDailySpendCents: 5000}},
	})
	for _, tc := range []struct {
		scope quota.ScopeType
		id    string
		limit quota.LimitType
		want  int64
	}{
		{quota.ScopeTenant, "t", quota.LimitDailySpendCents, 20000},
		{quota.ScopeUser, "u", quota.LimitDailySpendCents, 0},
		{quota.ScopeUser, "u", quota.LimitMonthlySpendCents, 25000},
		{quota.ScopeUser, "unset", quota.LimitMonthlySpendCents, 0},
		{quota.ScopeUser, "capped", quota.LimitDailySpendCents, 5000},
	} {
		if got := m.GetEffectiveLimit(tc.scope, tc.id, tc.limit); got != tc.want {
			t.Errorf("%s/%s/%s: got %d, want %d", tc.scope, tc.id, tc.limit, got, tc.want)
		}
	}
}

func TestValidatePolicy(t *testing.T) {
	tests := []struct {
		policy *quota.Policy
		valid  bool
	}{
		{&quota.Policy{ID: "p1", TenantID: "t1", ScopeType: quota.ScopeUser, ScopeID: "u1"}, true},
		{&quota.Policy{ID: "", TenantID: "t1", ScopeType: quota.ScopeUser, ScopeID: "u1"}, false},
		{&quota.Policy{ID: "p1", TenantID: "", ScopeType: quota.ScopeUser, ScopeID: "u1"}, false},
		{&quota.Policy{ID: "p1", TenantID: "t1", ScopeType: "invalid", ScopeID: "u1"}, false},
		{&quota.Policy{ID: "p1", TenantID: "t1", ScopeType: quota.ScopeTenant, ScopeID: ""}, false},
	}

	for _, tt := range tests {
		err := quota.ValidatePolicy(tt.policy)
		if tt.valid && err != nil {
			t.Errorf("expected valid, got: %v", err)
		}
		if !tt.valid && err == nil {
			t.Errorf("expected error for policy %+v", tt.policy)
		}
	}
}

func TestEstimateTokens(t *testing.T) {
	tokens := quota.EstimateTokens("hello world")
	if tokens <= 0 {
		t.Errorf("expected positive tokens, got %d", tokens)
	}
}

func TestTokenBucket(t *testing.T) {
	tb := quota.NewTokenBucket(10, 1)
	if !tb.Allow(5) {
		t.Error("expected allow with 5 tokens out of 10")
	}
	if !tb.Allow(5) {
		t.Error("expected allow with remaining 5 tokens")
	}
	if tb.Allow(5) {
		t.Error("expected deny when out of tokens")
	}
}

func TestInLimits(t *testing.T) {
	if !quota.InLimits("claude-opus-4", []string{"claude-opus-*"}) {
		t.Error("expected match for claude-opus-*")
	}
	if quota.InLimits("claude-sonnet-4", []string{"claude-opus-*"}) {
		t.Error("expected no match for claude-sonnet-4 against claude-opus-*")
	}
	if !quota.InLimits("gpt-4", []string{"gpt-4"}) {
		t.Error("expected exact match")
	}
}
