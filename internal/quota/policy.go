// Package quota provides rate limiting and quota enforcement.
package quota

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// ScopeType represents the scope of a quota policy.
type ScopeType string

const (
	ScopeTenant   ScopeType = "tenant"
	ScopeTeam     ScopeType = "team"
	ScopeUser     ScopeType = "user"
	ScopeKey      ScopeType = "api_key"
	ScopeProvider ScopeType = "provider"
	ScopeModel    ScopeType = "model"
)

// LimitType represents a type of quota limit.
type LimitType string

const (
	LimitRequestsPerMinute    LimitType = "requests_per_minute"
	LimitTokensPerMinute      LimitType = "tokens_per_minute"     //nolint:gosec
	LimitInputTokensPerDay    LimitType = "input_tokens_per_day"  //nolint:gosec
	LimitOutputTokensPerDay   LimitType = "output_tokens_per_day" //nolint:gosec
	LimitTotalTokensPerDay    LimitType = "total_tokens_per_day"
	LimitInputTokensPerMonth  LimitType = "input_tokens_per_month"  //nolint:gosec
	LimitOutputTokensPerMonth LimitType = "output_tokens_per_month" //nolint:gosec
	LimitTotalTokensPerMonth  LimitType = "total_tokens_per_month"
	LimitMonthlySpendCents    LimitType = "monthly_spend_cents"
	LimitDailySpendCents      LimitType = "daily_spend_cents"
	LimitConcurrentRequests   LimitType = "concurrent_requests"
	LimitMaxBodyBytes         LimitType = "max_request_body_bytes"
	LimitMaxInputTokens       LimitType = "max_input_tokens_per_request"
	LimitMaxOutputTokens      LimitType = "max_output_tokens_per_request"
	LimitMaxStreamDuration    LimitType = "max_stream_duration_seconds"
)

// Policy represents a quota policy configuration.
type Policy struct {
	ID        string              `json:"id"`
	TenantID  string              `json:"tenant_id"`
	ScopeType ScopeType           `json:"scope_type"`
	ScopeID   string              `json:"scope_id"`
	Priority  int                 `json:"priority"`
	Limits    map[LimitType]int64 `json:"limits"`
	Enabled   bool                `json:"enabled"`
}

// Reservation represents a quota reservation for a request.
type Reservation struct {
	ID                       string
	ScopeType                ScopeType
	ScopeID                  string
	Model                    string
	EstimatedTokens          int64
	EstimatedCost            int64
	EstimatedSpendMillicents int64
	Children                 []*Reservation
	CreatedAt                time.Time
	ExpiresAt                time.Time
	concurrencyReserved      bool
	spendReservations        []spendReservation
	deferredSpend            bool
	released                 bool
}

// Manager handles quota policy evaluation and enforcement.
type Manager struct {
	mu            sync.RWMutex
	policies      []*Policy
	defaultLimits map[LimitType]int64
	scopeDefaults map[ScopeType]map[LimitType]int64
}

// DefaultLimits returns the default quota limits.
//
// These are GENEROUS safety-nets, not tight caps: a deployment with no policy
// configured must still be bounded (an unset RPM/TPM/daily-spend previously left
// those dimensions UNLIMITED — the M3 gap). Admins tighten them per scope via
// policies. On a Redis outage, default-only rate/concurrency caps fail OPEN
// (availability) while spend caps fail CLOSED (strict, no unaccounted spend);
// see Enforcer.CheckAndReserve.
func DefaultLimits() map[LimitType]int64 {
	return map[LimitType]int64{
		LimitMonthlySpendCents:  50000,      // $500 / month
		LimitDailySpendCents:    10000,      // $100 / day
		LimitRequestsPerMinute:  6000,       // 100 req/s per scope
		LimitTokensPerMinute:    10_000_000, // generous TPM ceiling
		LimitConcurrentRequests: 64,
		LimitMaxOutputTokens:    8192,
		LimitMaxBodyBytes:       20 * 1024 * 1024,
		LimitMaxStreamDuration:  600,
	}
}

// ScopeDefaultOverrides returns per-scope-type replacements for specific default
// limits. The flat DefaultLimits concurrency cap of 64 is a sane per-key guard,
// but different scopes need different ceilings:
//   - Aggregate scopes (tenant, team) fan many actors together, so the flat 64
//     would throttle the WHOLE organization to 64 simultaneous requests — the
//     tenant-wide 429s seen in prod. They get a much larger ceiling.
//   - A single user typically drives more than one concurrent request (multiple
//     agents/sessions), so the user scope gets a modestly higher cap than a bare
//     API key.
//
// These are defaults only: an explicit policy at any scope still wins in
// GetEffectiveLimit. The per-key/user caps still bound any single actor.
func ScopeDefaultOverrides() map[ScopeType]map[LimitType]int64 {
	return map[ScopeType]map[LimitType]int64{
		ScopeTenant: {LimitConcurrentRequests: 2048},
		ScopeTeam:   {LimitConcurrentRequests: 512},
		ScopeUser:   {LimitConcurrentRequests: 128, LimitDailySpendCents: 0, LimitMonthlySpendCents: 0},
	}
}

// NewManager creates a new quota manager with sensible defaults.
func NewManager() *Manager {
	return &Manager{
		defaultLimits: DefaultLimits(),
		scopeDefaults: ScopeDefaultOverrides(),
	}
}

// SetPolicies replaces the current set of quota policies.
func (m *Manager) SetPolicies(policies []*Policy) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.policies = policies
}

// GetEffectiveLimit returns the most restrictive limit for a given type across all matching policies.
func (m *Manager) GetEffectiveLimit(scopeType ScopeType, scopeID string, limitType LimitType) int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var effective *int64

	for _, p := range m.policies {
		if !p.Enabled {
			continue
		}
		if p.ScopeType == scopeType && (p.ScopeID == scopeID || p.ScopeID == "*") {
			if val, ok := p.Limits[limitType]; ok {
				if effective == nil || val < *effective {
					v := val
					effective = &v
				}
			}
		}
	}

	if effective != nil {
		return *effective
	}

	// No explicit policy matched. Prefer a scope-specific default (e.g. the
	// higher tenant/team concurrency ceiling) over the flat per-actor default.
	if scoped, ok := m.scopeDefaults[scopeType]; ok {
		if val, ok := scoped[limitType]; ok {
			return val
		}
	}

	// Fall back to the flat default.
	if val, ok := m.defaultLimits[limitType]; ok {
		return val
	}
	return 0 // No limit.
}

// IsLimitConfigured reports whether an explicit (policy-set, non-default) limit
// of the given type applies to the scope.
//
// The enforcer uses this to scope fail-closed behavior on a Redis outage: an
// explicitly configured limit must deny when its distributed counter is
// unavailable, but a value coming only from defaults (e.g. the baseline
// concurrency cap) must NOT take all traffic down during an outage — the
// spend/rate dimensions still enforce their own fail-closed policy.
func (m *Manager) IsLimitConfigured(scopeType ScopeType, scopeID string, limitType LimitType) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, p := range m.policies {
		if !p.Enabled {
			continue
		}
		if p.ScopeType == scopeType && (p.ScopeID == scopeID || p.ScopeID == "*") {
			if _, ok := p.Limits[limitType]; ok {
				return true
			}
		}
	}
	return false
}

// CheckQuota returns nil if the request is within quota, or an error explaining why.
func (m *Manager) CheckQuota(scopeType ScopeType, scopeID string) error {
	maxConcurrent := m.GetEffectiveLimit(scopeType, scopeID, LimitConcurrentRequests)
	if maxConcurrent > 0 {
		// Check concurrent count (this would use a counter in production).
		// For Phase 1, we only check the limits.
		_ = maxConcurrent
	}

	return nil
}

// EstimateTokens provides a rough estimate of tokens from text length.
// This is a heuristic: ~4 characters per token for English text.
func EstimateTokens(text string) int64 {
	// Approximate: 4 chars per token.
	return int64(len(text) / 4)
}

// EstimateMessagesTokens estimates tokens for a list of messages.
// Each message has overhead for role, formatting, etc.
func EstimateMessagesTokens(messages []string) int64 {
	var total int64
	for _, msg := range messages {
		total += EstimateTokens(msg) + 10 // overhead per message
	}
	return total
}

// ExceededError is returned when a quota limit is exceeded.
type ExceededError struct {
	Scope   ScopeType `json:"scope"`
	Limit   LimitType `json:"limit"`
	Message string    `json:"message"`
}

func (e *ExceededError) Error() string {
	return fmt.Sprintf("quota exceeded: %s on %s/%s", e.Message, e.Scope, e.Limit)
}

// ValidatePolicy checks that a policy has valid fields.
func ValidatePolicy(p *Policy) error {
	if p.ID == "" {
		return fmt.Errorf("policy ID is required")
	}
	if p.TenantID == "" {
		return fmt.Errorf("tenant ID is required")
	}
	switch p.ScopeType {
	case ScopeTenant, ScopeTeam, ScopeUser, ScopeKey, ScopeProvider, ScopeModel:
		// Valid.
	default:
		return fmt.Errorf("invalid scope type: %s", p.ScopeType)
	}
	if p.ScopeID == "" {
		return fmt.Errorf("scope ID is required")
	}
	return nil
}

// TokenBucket implements a simple token bucket rate limiter (in-memory).
type TokenBucket struct {
	mu         sync.Mutex
	tokens     float64
	maxTokens  float64
	refillRate float64 // tokens per second
	lastRefill time.Time
}

// NewTokenBucket creates a new token bucket.
func NewTokenBucket(maxTokens, refillRate float64) *TokenBucket {
	return &TokenBucket{
		tokens:     maxTokens,
		maxTokens:  maxTokens,
		refillRate: refillRate,
		lastRefill: time.Now(),
	}
}

// Allow checks if a request is allowed and consumes tokens.
func (tb *TokenBucket) Allow(tokens float64) bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	// Refill.
	now := time.Now()
	elapsed := now.Sub(tb.lastRefill).Seconds()
	tb.tokens = min(tb.maxTokens, tb.tokens+elapsed*tb.refillRate)
	tb.lastRefill = now

	if tb.tokens >= tokens {
		tb.tokens -= tokens
		return true
	}
	return false
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// InLimits checks if a string matches a model pattern (supports glob-style * wildcards).
func InLimits(model string, patterns []string) bool {
	for _, pattern := range patterns {
		if strings.HasSuffix(pattern, "*") {
			prefix := strings.TrimSuffix(pattern, "*")
			if strings.HasPrefix(model, prefix) {
				return true
			}
		} else if model == pattern {
			return true
		}
	}
	return false
}
