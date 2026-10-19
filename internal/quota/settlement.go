package quota

import (
	"context"
	"fmt"
	"time"
)

const pendingSettlementBatchSize = 1000

// settleDeferredSpend writes exact post-request subscription charges to the
// durable outbox before applying them to Redis. The Redis operation is
// idempotent, so an ambiguous network error can be retried without double
// charging. Successfully applied rows are removed from the outbox.
func (e *Enforcer) settleDeferredSpend(ctx context.Context, resv *Reservation, amountMillicents int64) ([]spendCount, error) {
	if amountMillicents <= 0 || len(resv.spendReservations) == 0 {
		return nil, nil
	}
	if e.settlementStore == nil {
		return nil, fmt.Errorf("durable spend settlement store is unavailable")
	}
	if _, ok := e.redis.(IdempotentSpendCounterStore); !ok {
		return nil, fmt.Errorf("idempotent spend counter store is unavailable")
	}

	settlements := make([]SpendSettlement, 0, len(resv.spendReservations))
	for _, spend := range resv.spendReservations {
		settlements = append(settlements, SpendSettlement{
			ID:               resv.ID + ":" + string(spend.limit),
			ScopeType:        resv.ScopeType,
			ScopeID:          resv.ScopeID,
			CounterKey:       spend.key,
			Limit:            spend.limit,
			AmountMillicents: amountMillicents,
		})
	}
	if err := e.settlementStore.EnqueueSpendSettlements(ctx, settlements); err != nil {
		return nil, fmt.Errorf("enqueue subscription spend settlement: %w", err)
	}

	counts := make([]spendCount, 0, len(settlements))
	for _, settlement := range settlements {
		count, err := e.applySpendSettlement(ctx, settlement)
		if err != nil {
			return counts, err
		}
		counts = append(counts, spendCount{limit: settlement.Limit, count: count})
	}
	return counts, nil
}

// flushPendingSpendSettlements replays all outstanding charges for a scope
// before checking its current spend. Failure is fail-closed: admitting a new
// request against a stale Redis total would defeat the quota.
func (e *Enforcer) flushPendingSpendSettlements(ctx context.Context, scopeType ScopeType, scopeID string) error {
	if e.settlementStore == nil {
		return nil
	}
	for {
		pending, err := e.settlementStore.ListPendingSpendSettlements(ctx, scopeType, scopeID, pendingSettlementBatchSize)
		if err != nil {
			return fmt.Errorf("list pending spend settlements: %w", err)
		}
		if len(pending) == 0 {
			return nil
		}
		for _, settlement := range pending {
			if _, err := e.applySpendSettlement(ctx, settlement); err != nil {
				return err
			}
		}
		if len(pending) < pendingSettlementBatchSize {
			return nil
		}
	}
}

func (e *Enforcer) applySpendSettlement(ctx context.Context, settlement SpendSettlement) (int64, error) {
	ttl := spendCounterTTL(settlement.Limit, time.Now().UTC())
	var count int64
	var err error
	if settlement.Operation == SpendSettlementReconcileComponent {
		counter, ok := e.redis.(SpendComponentCounterStore)
		if !ok || counter == nil {
			return 0, fmt.Errorf("spend component store is unavailable")
		}
		count, err = counter.ReconcileSpendComponent(ctx, settlement.CounterKey, settlement.ComponentKey, settlement.ComponentVersion, settlement.AmountMillicents, ttl)
	} else if settlement.Operation == SpendSettlementEnsureMinimum {
		counter, ok := e.redis.(SpendFloorCounterStore)
		if !ok || counter == nil {
			return 0, fmt.Errorf("spend floor counter store is unavailable")
		}
		count, err = counter.EnsureSpendFloor(ctx, settlement.CounterKey, settlement.ID, settlement.AmountMillicents, ttl)
	} else {
		counter, ok := e.redis.(IdempotentSpendCounterStore)
		if !ok || counter == nil {
			return 0, fmt.Errorf("idempotent spend counter store is unavailable")
		}
		count, err = counter.ApplySpendSettlement(ctx, settlement.CounterKey, settlement.ID, settlement.AmountMillicents, ttl)
	}
	if err != nil {
		return 0, fmt.Errorf("apply subscription spend settlement %s: %w", settlement.ID, err)
	}
	if err := e.settlementStore.DeleteSpendSettlement(ctx, settlement.ID); err != nil {
		// Redis remembers the settlement ID, so leaving the row pending is safe:
		// the next replay observes the same total without incrementing it again.
		return count, fmt.Errorf("delete applied subscription spend settlement %s: %w", settlement.ID, err)
	}
	return count, nil
}

func spendCounterTTL(limit LimitType, now time.Time) time.Duration {
	switch limit {
	case LimitDailySpendCents:
		return time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC).Sub(now)
	case LimitMonthlySpendCents:
		return time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC).Sub(now)
	default:
		return time.Hour
	}
}
