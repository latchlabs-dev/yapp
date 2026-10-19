package quota

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/earlgrey/piproxy/internal/metrics"
)

// ErrRedisUnavailable is returned by the internal counter helpers when Redis is
// nil or disconnected and a limit is at stake. It funnels disconnected
// spend/rate/concurrency checks through the fail-closed (deny) path instead of
// silently allowing the request. Spend and rate limits are enforcement
// dimensions: when we cannot consult the distributed counter we must DENY
// rather than fall back to per-instance behaviour (which would permit unbounded
// cluster-wide spend / N*K concurrency).
var ErrRedisUnavailable = errors.New("quota: redis counter store unavailable")

// CounterStore is the Redis-facing counter API used by the quota enforcer.
type CounterStore interface {
	IsConnected() bool
	ConcurrencyIncr(ctx context.Context, key string) (int64, error)
	ConcurrencyDecr(ctx context.Context, key string) (int64, error)
	SlidingWindowAdd(ctx context.Context, key string, window time.Duration, amount int64) (int64, error)
	DailyAdd(ctx context.Context, key string, amount int64) (int64, error)
	MonthlyAdd(ctx context.Context, key string, amount int64) (int64, error)
}

// IdempotentSpendCounterStore atomically applies one durable settlement at
// most once. Implementations retain the settlement ID for the counter period.
type IdempotentSpendCounterStore interface {
	ApplySpendSettlement(ctx context.Context, counterKey, settlementID string, amountMillicents int64, ttl time.Duration) (int64, error)
}

// SpendFloorCounterStore raises a counter to an absolute minimum without
// incrementing it when the existing total is already higher. It reconciles
// Redis from the durable Postgres usage ledger without double charging.
type SpendFloorCounterStore interface {
	EnsureSpendFloor(ctx context.Context, counterKey, settlementID string, minimumMillicents int64, ttl time.Duration) (int64, error)
}

// Component targets allow both credits and debits without overwriting other
// providers' costs or in-flight reservations in the same quota counter.
type SpendComponentCounterStore interface {
	ReconcileSpendComponent(context.Context, string, string, int64, int64, time.Duration) (int64, error)
}

type SpendSettlementOperation string

const (
	SpendSettlementAdd                SpendSettlementOperation = "add"
	SpendSettlementEnsureMinimum      SpendSettlementOperation = "ensure_minimum"
	SpendSettlementReconcileComponent SpendSettlementOperation = "reconcile_component"
)

// SpendSettlement is one exact post-request counter adjustment. It is stored
// durably before Redis is touched so transient Redis failures cannot lose it.
type SpendSettlement struct {
	ID               string
	ScopeType        ScopeType
	ScopeID          string
	CounterKey       string
	Limit            LimitType
	AmountMillicents int64
	Operation        SpendSettlementOperation
	ComponentKey     string
	ComponentVersion int64
}

// SpendSettlementStore is the durable outbox used by deferred subscription
// charging. Pending entries are replayed before another request is admitted.
type SpendSettlementStore interface {
	EnqueueSpendSettlements(ctx context.Context, settlements []SpendSettlement) error
	ListPendingSpendSettlements(ctx context.Context, scopeType ScopeType, scopeID string, limit int) ([]SpendSettlement, error)
	DeleteSpendSettlement(ctx context.Context, id string) error
}

// SpendCounterScale is the fixed-point scale used for spend counters.
// Policies are configured in cents; Redis counters store millicents so small
// requests do not round up to a full cent.
const SpendCounterScale int64 = 1000

type spendReservation struct {
	key              string
	limit            LimitType
	amountMillicents int64
}

// ReconciliationOverageError is returned from Complete when actual spend
// exceeds the up-front reservation enough to push a spend counter over its cap.
// The request has already completed, so callers cannot reject it, but the error
// makes the breach visible to logs/hooks while the over-cap Redis total denies
// subsequent reservations.
type ReconciliationOverageError struct {
	Scope           ScopeType
	Limit           LimitType
	CountMillicents int64
	LimitMillicents int64
}

func (e *ReconciliationOverageError) Error() string {
	return fmt.Sprintf("%s %s quota exceeded during spend reconciliation (%d > %d millicents)", e.Scope, e.Limit, e.CountMillicents, e.LimitMillicents)
}

// Enforcer checks and enforces quota limits using Redis-backed counters.
// It coordinates the quota.Manager (policy evaluation) with the Redis counter
// store (distributed rate limiting).
type Enforcer struct {
	manager         *Manager
	redis           CounterStore
	quotaMetrics    *metrics.QuotaMetrics
	settlementStore SpendSettlementStore
}

// SetSpendSettlementStore enables durable deferred-spend settlement. The proxy
// configures this with Postgres before accepting requests.
func (e *Enforcer) SetSpendSettlementStore(store SpendSettlementStore) {
	if e != nil {
		e.settlementStore = store
	}
}

// NewEnforcer creates a new quota enforcer.
// The redis client may be disconnected (nil-safe); each quota dimension applies
// its documented fail-open or fail-closed behavior in that case.
func NewEnforcer(manager *Manager, redisClient CounterStore, quotaMetrics *metrics.QuotaMetrics) *Enforcer {
	return &Enforcer{
		manager:      manager,
		redis:        redisClient,
		quotaMetrics: quotaMetrics,
	}
}

// CheckAndReserve checks whether a request is within quota and reserves
// capacity. Spend reservations are passed in millicents as an optional final
// argument to keep older call sites source-compatible.
func (e *Enforcer) CheckAndReserve(ctx context.Context, scopeType ScopeType, scopeID, model string, estimatedTokens int64, estimatedSpendMillicentsOpt ...int64) (*Reservation, error) {
	return e.checkAndReserve(ctx, scopeType, scopeID, model, estimatedTokens, false, estimatedSpendMillicentsOpt...)
}

// CheckAndReserveDeferredSpend checks the current spend counters without
// reserving estimated money. The completed request is charged in Complete;
// once that pushes a counter to its limit, the next request is denied.
func (e *Enforcer) CheckAndReserveDeferredSpend(ctx context.Context, scopeType ScopeType, scopeID, model string, estimatedTokens int64) (*Reservation, error) {
	return e.checkAndReserve(ctx, scopeType, scopeID, model, estimatedTokens, true)
}

func (e *Enforcer) checkAndReserve(ctx context.Context, scopeType ScopeType, scopeID, model string, estimatedTokens int64, deferredSpend bool, estimatedSpendMillicentsOpt ...int64) (*Reservation, error) {
	now := time.Now()
	if deferredSpend && e.settlementStore != nil {
		if err := e.flushPendingSpendSettlements(ctx, scopeType, scopeID); err != nil {
			e.observeQuota(scopeType, "denied:pending_spend_settlement")
			return nil, fmt.Errorf("pending subscription spend settlement unavailable: %w", err)
		}
	}
	estimatedSpendMillicents := int64(0)
	if len(estimatedSpendMillicentsOpt) > 0 {
		estimatedSpendMillicents = estimatedSpendMillicentsOpt[0]
	}

	// 1. Check max input tokens before mutating any distributed counters.
	if estimatedTokens > 0 {
		maxInputLimit := e.manager.GetEffectiveLimit(scopeType, scopeID, LimitMaxInputTokens)
		if maxInputLimit > 0 && estimatedTokens > maxInputLimit {
			e.observeQuota(scopeType, "denied:max_input_tokens")
			return nil, &ExceededError{
				Scope:   scopeType,
				Limit:   LimitMaxInputTokens,
				Message: fmt.Sprintf("input token limit %d exceeded (estimated %d)", maxInputLimit, estimatedTokens),
			}
		}
	}

	// 2. Check concurrent requests.
	concurrencyKey := fmt.Sprintf("quota:concurrency:%s:%s", scopeType, scopeID)
	concurrencyLimit := e.manager.GetEffectiveLimit(scopeType, scopeID, LimitConcurrentRequests)
	concurrencyReserved := false
	if concurrencyLimit > 0 {
		count, err := e.incrConcurrency(ctx, concurrencyKey)
		if err != nil {
			// Redis is unavailable. Fail CLOSED only when the concurrency cap is
			// EXPLICITLY configured for this scope: an admin who set a concurrency
			// limit wants it enforced cluster-wide, never per-instance. A
			// default-only cap, by contrast, must not deny all traffic on a Redis
			// blip — fall through so the spend/rate dimensions below apply their
			// own (independent) fail-closed policy.
			if e.manager.IsLimitConfigured(scopeType, scopeID, LimitConcurrentRequests) {
				e.observeQuota(scopeType, "denied:redis_unavailable")
				return nil, &ExceededError{
					Scope:   scopeType,
					Limit:   LimitConcurrentRequests,
					Message: "concurrency quota enforcement unavailable (redis down)",
				}
			}
		} else {
			if count > concurrencyLimit {
				_, _ = e.decrConcurrency(ctx, concurrencyKey)
				e.observeQuota(scopeType, "denied:concurrency")
				return nil, &ExceededError{
					Scope:   scopeType,
					Limit:   LimitConcurrentRequests,
					Message: fmt.Sprintf("concurrent requests limit %d exceeded (%d active)", concurrencyLimit, count-1),
				}
			}
			concurrencyReserved = true
		}
	}

	nowUTC := now.UTC()
	dateStr := nowUTC.Format("2006-01-02")
	monthStr := nowUTC.Format("2006-01")

	// 3. Reserve daily/monthly spend before lower-priority rate/token controls.
	// Spend counters are fixed-point millicents; policy limits remain cents for
	// admin ergonomics.
	//
	// Spend is the PRIMARY enforcement dimension, so it must fail CLOSED: if a
	// spend limit is configured but the distributed counter is unavailable (or
	// an INCR/EXPIRE errors), we deny rather than allow an unaccounted spend.
	// The guard is split so the connectivity check is per spend-limit: a request
	// with NO spend limit configured is never blocked by Redis being down.
	var spendReservations []spendReservation
	if estimatedSpendMillicents > 0 || deferredSpend {
		redisUp := e.redis != nil && e.redis.IsConnected()

		dailySpendLimit := e.manager.GetEffectiveLimit(scopeType, scopeID, LimitDailySpendCents)
		if dailySpendLimit > 0 {
			if !redisUp {
				// Fail closed: spend is the PRIMARY dimension, so we deny rather
				// than allow unaccounted spend when the counter store is down —
				// for default and explicit caps alike (strict H1 policy). Only a
				// request with NO spend at stake (estimatedSpend == 0) skips this.
				e.refundSpend(ctx, spendReservations)
				e.releaseConcurrency(ctx, concurrencyKey, concurrencyReserved)
				e.observeQuota(scopeType, "denied:redis_unavailable")
				return nil, &ExceededError{
					Scope:   scopeType,
					Limit:   LimitDailySpendCents,
					Message: "daily spend quota enforcement unavailable (redis down)",
				}
			} else {
				key := fmt.Sprintf("quota:daily:spend_millicents:%s:%s:%s", scopeType, scopeID, dateStr)
				amount := estimatedSpendMillicents
				if deferredSpend {
					amount = 0
				}
				count, err := e.redis.DailyAdd(ctx, key, amount)
				if err != nil {
					// Fail closed on counter error: we cannot trust the daily total.
					e.refundSpend(ctx, spendReservations)
					e.releaseConcurrency(ctx, concurrencyKey, concurrencyReserved)
					e.observeQuota(scopeType, "denied:redis_error")
					return nil, fmt.Errorf("daily spend counter unavailable: %w", err)
				}
				res := spendReservation{key: key, limit: LimitDailySpendCents, amountMillicents: amount}
				spendReservations = append(spendReservations, res)
				if (!deferredSpend && count > scaleSpendLimit(dailySpendLimit)) ||
					(deferredSpend && count >= scaleSpendLimit(dailySpendLimit)) {
					e.refundSpend(ctx, spendReservations)
					e.releaseConcurrency(ctx, concurrencyKey, concurrencyReserved)
					e.observeQuota(scopeType, "denied:daily_spend")
					return nil, &ExceededError{
						Scope:   scopeType,
						Limit:   LimitDailySpendCents,
						Message: fmt.Sprintf("daily spend limit %s exceeded", formatCents(dailySpendLimit)),
					}
				}
			}
		}

		monthlySpendLimit := e.manager.GetEffectiveLimit(scopeType, scopeID, LimitMonthlySpendCents)
		if monthlySpendLimit > 0 {
			if !redisUp {
				// Fail closed for default and explicit monthly caps alike (strict
				// H1 policy): a spend-bearing request must not proceed unaccounted
				// during a Redis outage.
				// (spendReservations may already hold the daily reservation; refund
				// rolls it back — Redis is up in the mixed case, down here means it
				// was never persisted.)
				e.refundSpend(ctx, spendReservations)
				e.releaseConcurrency(ctx, concurrencyKey, concurrencyReserved)
				e.observeQuota(scopeType, "denied:redis_unavailable")
				return nil, &ExceededError{
					Scope:   scopeType,
					Limit:   LimitMonthlySpendCents,
					Message: "monthly spend quota enforcement unavailable (redis down)",
				}
			} else {
				key := fmt.Sprintf("quota:monthly:spend_millicents:%s:%s:%s", scopeType, scopeID, monthStr)
				amount := estimatedSpendMillicents
				if deferredSpend {
					amount = 0
				}
				count, err := e.redis.MonthlyAdd(ctx, key, amount)
				if err != nil {
					// Fail closed on counter error. The daily reservation (if any) is
					// already in spendReservations, so refundSpend rolls it back.
					e.refundSpend(ctx, spendReservations)
					e.releaseConcurrency(ctx, concurrencyKey, concurrencyReserved)
					e.observeQuota(scopeType, "denied:redis_error")
					return nil, fmt.Errorf("monthly spend counter unavailable: %w", err)
				}
				res := spendReservation{key: key, limit: LimitMonthlySpendCents, amountMillicents: amount}
				spendReservations = append(spendReservations, res)
				if (!deferredSpend && count > scaleSpendLimit(monthlySpendLimit)) ||
					(deferredSpend && count >= scaleSpendLimit(monthlySpendLimit)) {
					e.refundSpend(ctx, spendReservations)
					e.releaseConcurrency(ctx, concurrencyKey, concurrencyReserved)
					e.observeQuota(scopeType, "denied:monthly_spend")
					return nil, &ExceededError{
						Scope:   scopeType,
						Limit:   LimitMonthlySpendCents,
						Message: fmt.Sprintf("monthly spend limit %s exceeded", formatCents(monthlySpendLimit)),
					}
				}
			}
		}
		// If no spend limit is configured, there is nothing to enforce, so a
		// Redis outage is harmless here and we deliberately fall through (fail
		// open) — no spend is at stake.
	}

	// 4. Check requests per minute (sliding window).
	rpmKey := fmt.Sprintf("quota:rpm:%s:%s", scopeType, scopeID)
	rpmLimit := e.manager.GetEffectiveLimit(scopeType, scopeID, LimitRequestsPerMinute)
	if rpmLimit > 0 {
		count, err := e.slidingWindowAdd(ctx, rpmKey, 1*time.Minute, 1)
		if err != nil {
			// We cannot consult the shared sliding window (Redis down/errored).
			// Fail CLOSED only for an EXPLICITLY configured RPM cap — leaving an
			// admin-set rate limit unenforced (or per-instance) is worse than a
			// brief deny. A default-only cap falls through (fail open) so a Redis
			// blip cannot deny ALL traffic cluster-wide; spend stays strict above.
			if e.manager.IsLimitConfigured(scopeType, scopeID, LimitRequestsPerMinute) {
				e.refundSpend(ctx, spendReservations)
				e.releaseConcurrency(ctx, concurrencyKey, concurrencyReserved)
				e.observeQuota(scopeType, "denied:redis_error")
				return nil, fmt.Errorf("rpm rate counter unavailable: %w", err)
			}
		} else if count > rpmLimit {
			e.refundSpend(ctx, spendReservations)
			e.releaseConcurrency(ctx, concurrencyKey, concurrencyReserved)
			e.observeQuota(scopeType, "denied:rpm")
			return nil, &ExceededError{
				Scope:   scopeType,
				Limit:   LimitRequestsPerMinute,
				Message: fmt.Sprintf("RPM limit %d exceeded (%d this minute)", rpmLimit, count),
			}
		}
	}

	// 5. Check tokens per minute (sliding window, only if we have tokens to estimate).
	if estimatedTokens > 0 {
		tpmKey := fmt.Sprintf("quota:tpm:%s:%s", scopeType, scopeID)
		tpmLimit := e.manager.GetEffectiveLimit(scopeType, scopeID, LimitTokensPerMinute)
		if tpmLimit > 0 {
			count, err := e.slidingWindowAdd(ctx, tpmKey, 1*time.Minute, estimatedTokens)
			if err != nil {
				// Fail CLOSED only for an EXPLICIT TPM cap; a default-only cap
				// falls through (fail open) so a Redis blip cannot deny all
				// traffic. (Spend dimensions above remain strict.)
				if e.manager.IsLimitConfigured(scopeType, scopeID, LimitTokensPerMinute) {
					e.refundSpend(ctx, spendReservations)
					e.releaseConcurrency(ctx, concurrencyKey, concurrencyReserved)
					e.observeQuota(scopeType, "denied:redis_error")
					return nil, fmt.Errorf("tpm rate counter unavailable: %w", err)
				}
			} else if count > tpmLimit {
				e.refundSpend(ctx, spendReservations)
				e.releaseConcurrency(ctx, concurrencyKey, concurrencyReserved)
				e.observeQuota(scopeType, "denied:tpm")
				return nil, &ExceededError{
					Scope:   scopeType,
					Limit:   LimitTokensPerMinute,
					Message: fmt.Sprintf("TPM limit %d exceeded", tpmLimit),
				}
			}
		}
	}

	// 6. Check daily/monthly token totals when explicitly configured. These are
	// enforcement limits, so they fail CLOSED: a configured token-total limit
	// that cannot be checked (Redis down or counter error) denies the request.
	// The connectivity check is per-limit so scopes with no token-total limit
	// configured are unaffected by a Redis outage.
	if estimatedTokens > 0 {
		redisUp := e.redis != nil && e.redis.IsConnected()

		dailyTotalLimit := e.manager.GetEffectiveLimit(scopeType, scopeID, LimitTotalTokensPerDay)
		if dailyTotalLimit > 0 {
			if !redisUp {
				e.refundSpend(ctx, spendReservations)
				e.releaseConcurrency(ctx, concurrencyKey, concurrencyReserved)
				e.observeQuota(scopeType, "denied:redis_unavailable")
				return nil, &ExceededError{
					Scope:   scopeType,
					Limit:   LimitTotalTokensPerDay,
					Message: "daily token quota enforcement unavailable (redis down)",
				}
			}
			dailyTotalKey := fmt.Sprintf("quota:daily:tokens:%s:%s:%s", scopeType, scopeID, dateStr)
			count, err := e.redis.DailyAdd(ctx, dailyTotalKey, estimatedTokens)
			if err != nil {
				e.refundSpend(ctx, spendReservations)
				e.releaseConcurrency(ctx, concurrencyKey, concurrencyReserved)
				e.observeQuota(scopeType, "denied:redis_error")
				return nil, fmt.Errorf("daily token counter unavailable: %w", err)
			}
			if count > dailyTotalLimit {
				e.refundSpend(ctx, spendReservations)
				e.releaseConcurrency(ctx, concurrencyKey, concurrencyReserved)
				e.observeQuota(scopeType, "denied:daily_tokens")
				return nil, &ExceededError{
					Scope:   scopeType,
					Limit:   LimitTotalTokensPerDay,
					Message: fmt.Sprintf("daily token limit %d exceeded", dailyTotalLimit),
				}
			}
		}

		monthlyTotalLimit := e.manager.GetEffectiveLimit(scopeType, scopeID, LimitTotalTokensPerMonth)
		if monthlyTotalLimit > 0 {
			if !redisUp {
				e.refundSpend(ctx, spendReservations)
				e.releaseConcurrency(ctx, concurrencyKey, concurrencyReserved)
				e.observeQuota(scopeType, "denied:redis_unavailable")
				return nil, &ExceededError{
					Scope:   scopeType,
					Limit:   LimitTotalTokensPerMonth,
					Message: "monthly token quota enforcement unavailable (redis down)",
				}
			}
			monthlyTotalKey := fmt.Sprintf("quota:monthly:tokens:%s:%s:%s", scopeType, scopeID, monthStr)
			count, err := e.redis.MonthlyAdd(ctx, monthlyTotalKey, estimatedTokens)
			if err != nil {
				e.refundSpend(ctx, spendReservations)
				e.releaseConcurrency(ctx, concurrencyKey, concurrencyReserved)
				e.observeQuota(scopeType, "denied:redis_error")
				return nil, fmt.Errorf("monthly token counter unavailable: %w", err)
			}
			if count > monthlyTotalLimit {
				e.refundSpend(ctx, spendReservations)
				e.releaseConcurrency(ctx, concurrencyKey, concurrencyReserved)
				e.observeQuota(scopeType, "denied:monthly_tokens")
				return nil, &ExceededError{
					Scope:   scopeType,
					Limit:   LimitTotalTokensPerMonth,
					Message: fmt.Sprintf("monthly token limit %d exceeded", monthlyTotalLimit),
				}
			}
		}
	}

	// We have a valid reservation.
	e.observeQuota(scopeType, "allow")
	return &Reservation{
		ID:                       fmt.Sprintf("%s-%s-%d", scopeType, scopeID, now.UnixNano()),
		ScopeType:                scopeType,
		ScopeID:                  scopeID,
		Model:                    model,
		EstimatedTokens:          estimatedTokens,
		EstimatedCost:            estimatedSpendMillicents,
		EstimatedSpendMillicents: estimatedSpendMillicents,
		concurrencyReserved:      concurrencyReserved,
		spendReservations:        spendReservations,
		deferredSpend:            deferredSpend,
		CreatedAt:                now,
		ExpiresAt:                now.Add(30 * time.Second),
	}, nil
}

// Complete releases a reservation and reconciles actual usage. Actual spend is
// passed in millicents as an optional final argument; when omitted the original
// reservation amount is retained.
func (e *Enforcer) Complete(ctx context.Context, resv *Reservation, actualTokens int64, actualSpendMillicentsOpt ...int64) error {
	if resv == nil || resv.released {
		return nil
	}
	resv.released = true

	var firstErr error
	for _, child := range resv.Children {
		if err := e.Complete(ctx, child, actualTokens, actualSpendMillicentsOpt...); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if len(resv.Children) > 0 {
		return firstErr
	}

	// Release concurrency slot.
	concurrencyKey := fmt.Sprintf("quota:concurrency:%s:%s", resv.ScopeType, resv.ScopeID)
	e.releaseConcurrency(ctx, concurrencyKey, resv.concurrencyReserved)

	actualSpendMillicents := resv.EstimatedSpendMillicents
	if len(actualSpendMillicentsOpt) > 0 {
		actualSpendMillicents = actualSpendMillicentsOpt[0]
	}
	if delta := actualSpendMillicents - resv.EstimatedSpendMillicents; delta != 0 {
		var counts []spendCount
		if resv.deferredSpend && delta > 0 {
			var err error
			counts, err = e.settleDeferredSpend(ctx, resv, delta)
			if err != nil {
				e.observeQuota(resv.ScopeType, "settlement_error")
				if firstErr == nil {
					firstErr = err
				}
			}
		} else {
			counts = e.adjustSpend(ctx, resv.spendReservations, delta)
		}
		if delta > 0 {
			// Reconciliation pushed the counter UP because actual spend exceeded
			// the up-front estimate. We cannot deny a request that already
			// completed, but if the reconciled total now exceeds the cap we
			// surface it as a metric so the estimate error is visible — and the
			// next request on this scope will be denied by the now-over counter.
			for _, sc := range counts {
				limit := e.manager.GetEffectiveLimit(resv.ScopeType, resv.ScopeID, sc.limit)
				limitMillicents := scaleSpendLimit(limit)
				if limit > 0 && sc.count > limitMillicents {
					e.observeQuota(resv.ScopeType, "reconcile_overage")
					if firstErr == nil {
						firstErr = &ReconciliationOverageError{
							Scope:           resv.ScopeType,
							Limit:           sc.limit,
							CountMillicents: sc.count,
							LimitMillicents: limitMillicents,
						}
					}
				}
			}
		}
	}

	// Actual token accounting is done via Postgres usage events. Spend counters
	// are reconciled here because spend is the primary enforcement dimension.
	_ = actualTokens
	return firstErr
}

// Rollback releases a reservation without recording usage.
func (e *Enforcer) Rollback(ctx context.Context, resv *Reservation) error {
	if resv == nil || resv.released {
		return nil
	}
	resv.released = true

	for _, child := range resv.Children {
		if err := e.Rollback(ctx, child); err != nil {
			return err
		}
	}
	if len(resv.Children) > 0 {
		return nil
	}

	concurrencyKey := fmt.Sprintf("quota:concurrency:%s:%s", resv.ScopeType, resv.ScopeID)
	e.releaseConcurrency(ctx, concurrencyKey, resv.concurrencyReserved)
	e.refundSpend(ctx, resv.spendReservations)

	return nil
}

// EffectiveLimit exposes policy resolution to the data plane for request-cost
// estimation without letting handlers import the manager directly.
func (e *Enforcer) EffectiveLimit(scopeType ScopeType, scopeID string, limitType LimitType) int64 {
	if e == nil || e.manager == nil {
		return 0
	}
	return e.manager.GetEffectiveLimit(scopeType, scopeID, limitType)
}

// incrConcurrency increments the distributed concurrency counter.
//
// When Redis is unavailable we FAIL CLOSED (return ErrRedisUnavailable) rather
// than fall back to the per-instance in-memory map: a concurrency limit of K
// across N proxy instances would otherwise degrade to an effective cluster
// limit of N*K, which is exactly the "per-instance concurrency counters"
// problem H1 calls out. The caller (CheckAndReserve) treats this error as a
// deny. The in-memory map is retained only as the decrement-side bookkeeping
// for any counts taken while Redis was up.
func (e *Enforcer) incrConcurrency(ctx context.Context, key string) (int64, error) {
	if e.redis != nil && e.redis.IsConnected() {
		return e.redis.ConcurrencyIncr(ctx, key)
	}
	return 0, ErrRedisUnavailable
}

// decrConcurrency decrements the distributed concurrency counter.
//
// Concurrency slots live ONLY in Redis — incrConcurrency never touches a local
// map (it fails closed when Redis is down), so the decrement must target Redis
// exclusively. The previous in-memory fallback decremented a map the increment
// never populated, which (a) underflowed the local map and (b) left the Redis
// counter inflated across a connect/disconnect — the counter DRIFT M4 calls
// out. When Redis is unavailable at release time we skip the decrement and let
// the 15-minute safety TTL on the key reclaim the slot, recording a metric so
// the bounded, self-healing drift stays observable.
func (e *Enforcer) decrConcurrency(ctx context.Context, key string) (int64, error) {
	if e.redis != nil && e.redis.IsConnected() {
		return e.redis.ConcurrencyDecr(ctx, key)
	}
	e.observeQuota(scopeFromConcurrencyKey(key), "concurrency_release_skipped_redis_down")
	return 0, ErrRedisUnavailable
}

// scopeFromConcurrencyKey extracts the scope type from a concurrency key of the
// form "quota:concurrency:<scopeType>:<scopeID>" for metric labelling; returns
// "unknown" if the key is not in the expected shape.
func scopeFromConcurrencyKey(key string) ScopeType {
	parts := strings.Split(key, ":")
	if len(parts) >= 3 {
		return ScopeType(parts[2])
	}
	return ScopeType("unknown")
}

// slidingWindowAdd increments a sliding window counter by amount.
//
// It only runs when a RPM/TPM limit is configured (the callers gate on
// limit > 0). When Redis is unavailable we return ErrRedisUnavailable instead
// of fabricating a (0, nil) success, so the caller fails CLOSED: a disconnected
// Redis must not silently pass a rate-limited request (count 0 < limit) with no
// error. There is no per-instance in-memory fallback because that would make
// the rate limit per-instance rather than cluster-wide.
func (e *Enforcer) slidingWindowAdd(ctx context.Context, key string, window time.Duration, amount int64) (int64, error) {
	if e.redis != nil && e.redis.IsConnected() {
		return e.redis.SlidingWindowAdd(ctx, key, window, amount)
	}
	return 0, ErrRedisUnavailable
}

func (e *Enforcer) releaseConcurrency(ctx context.Context, key string, reserved bool) {
	if reserved {
		_, _ = e.decrConcurrency(ctx, key)
	}
}

func (e *Enforcer) refundSpend(ctx context.Context, reservations []spendReservation) {
	for _, res := range reservations {
		e.adjustSpend(ctx, []spendReservation{res}, -res.amountMillicents)
	}
}

// spendCount is the post-adjustment total for one spend reservation, returned so
// Complete can re-check reconciled spend against its cap.
type spendCount struct {
	limit LimitType
	count int64
}

func (e *Enforcer) adjustSpend(ctx context.Context, reservations []spendReservation, deltaMillicents int64) []spendCount {
	if deltaMillicents == 0 || e.redis == nil || !e.redis.IsConnected() {
		return nil
	}
	counts := make([]spendCount, 0, len(reservations))
	for _, res := range reservations {
		var c int64
		switch res.limit {
		case LimitDailySpendCents:
			c, _ = e.redis.DailyAdd(ctx, res.key, deltaMillicents)
		case LimitMonthlySpendCents:
			c, _ = e.redis.MonthlyAdd(ctx, res.key, deltaMillicents)
		}
		counts = append(counts, spendCount{limit: res.limit, count: c})
	}
	return counts
}

func (e *Enforcer) observeQuota(scopeType ScopeType, decision string) {
	if e.quotaMetrics != nil {
		e.quotaMetrics.ObserveQuota(string(scopeType), decision)
	}
}

func scaleSpendLimit(limitCents int64) int64 {
	return limitCents * SpendCounterScale
}

func formatCents(cents int64) string {
	dollars := cents / 100
	remainder := cents % 100
	if remainder < 0 {
		remainder = -remainder
	}
	return fmt.Sprintf("$%d.%02d", dollars, remainder)
}
