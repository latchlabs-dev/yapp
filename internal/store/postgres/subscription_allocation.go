package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/earlgrey/piproxy/internal/providerbalance"
	"github.com/earlgrey/piproxy/internal/quota"
	"github.com/earlgrey/piproxy/internal/subscriptioncost"
)

// CreateBillingPeriod records verified paid dates. Account row locking also
// serializes overlap validation, including concurrent first-period creation.
func (s *SubscriptionRateStore) CreateBillingPeriod(ctx context.Context, p subscriptioncost.Period) error {
	return s.createBillingPeriod(ctx, p, false)
}

// CreateHistoricalBillingPeriod is for the maintenance command, with request
// traffic and both allocator processes stopped until counters are rebuilt.
func (s *SubscriptionRateStore) CreateHistoricalBillingPeriod(ctx context.Context, p subscriptioncost.Period) error {
	return s.createBillingPeriod(ctx, p, true)
}

func (s *SubscriptionRateStore) createBillingPeriod(ctx context.Context, p subscriptioncost.Period, historical bool) error {
	if p.ProviderAccountID == "" || p.Amount <= 0 || p.Amount > 1_000_000_000_000 || p.Start.IsZero() || !p.End.After(p.Start) {
		return fmt.Errorf("provider, positive amount and paid start/end are required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var mode, currency string
	if err = tx.QueryRow(ctx, `SELECT COALESCE(config->'billing'->>'mode',''),COALESCE(config->'billing'->>'currency','USD') FROM provider_accounts WHERE id=$1 FOR UPDATE`, p.ProviderAccountID).Scan(&mode, &currency); err != nil {
		return err
	}
	if mode != "subscription" {
		return fmt.Errorf("provider is not a subscription")
	}
	if currency != "USD" {
		return fmt.Errorf("observed allocation currently requires USD")
	}
	if !historical {
		var oldCosts bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM usage_events WHERE provider_account_id=$1 AND completed_at>=$2 AND completed_at<$3 AND cost_total_cents>0)`, p.ProviderAccountID, p.Start, p.End).Scan(&oldCosts); err != nil {
			return err
		}
		if oldCosts {
			return fmt.Errorf("existing charges require the historical maintenance command and quota-counter reconciliation")
		}
	}
	var overlap bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM subscription_billing_periods WHERE provider_account_id=$1 AND starts_at<$3 AND ends_at>$2)`, p.ProviderAccountID, p.Start, p.End).Scan(&overlap); err != nil {
		return err
	}
	if overlap {
		return fmt.Errorf("paid period overlaps an existing period")
	}
	if _, err = tx.Exec(ctx, `INSERT INTO subscription_billing_periods(provider_account_id,starts_at,ends_at,amount_millicents) VALUES($1,$2,$3,$4)`, p.ProviderAccountID, p.Start, p.End, p.Amount); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE provider_accounts SET config=jsonb_set(config,'{billing,allocation_method}','"observed_delta"'),updated_at=now() WHERE id=$1`, p.ProviderAccountID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

const billingPeriodSelect = `SELECT id,provider_account_id,starts_at,ends_at,amount_millicents,allocated_millicents,unattributed_millicents,consumed_points::double precision,revision,observed_through,evidence_hash FROM subscription_billing_periods`

func scanBillingPeriod(row rateRowScanner) (subscriptioncost.Period, error) {
	var p subscriptioncost.Period
	err := row.Scan(&p.ID, &p.ProviderAccountID, &p.Start, &p.End, &p.Amount, &p.Allocated, &p.Unattributed, &p.ConsumedPoints, &p.Revision, &p.ObservedThrough, &p.EvidenceHash)
	return p, err
}

func (s *SubscriptionRateStore) ListBillingPeriods(ctx context.Context, providerID string) ([]subscriptioncost.Period, error) {
	rows, err := s.pool.Query(ctx, billingPeriodSelect+` WHERE ($1='' OR provider_account_id=$1) ORDER BY starts_at,id`, providerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	periods := []subscriptioncost.Period{}
	for rows.Next() {
		p, err := scanBillingPeriod(rows)
		if err != nil {
			return nil, err
		}
		periods = append(periods, p)
	}
	return periods, rows.Err()
}

func (s *SubscriptionRateStore) ReconcileObservedSubscriptions(ctx context.Context, asOf time.Time) error {
	periods, err := s.ListBillingPeriods(ctx, "")
	if err != nil {
		return err
	}
	for _, p := range periods {
		if p.Start.Before(asOf) {
			if _, err = s.ReconcileBillingPeriod(ctx, p.ID, asOf, true); err != nil {
				return fmt.Errorf("subscription period %d: %w", p.ID, err)
			}
		}
	}
	return nil
}

type allocationEvent struct {
	subscriptioncost.AllocationRequest
	TenantID, UserID, KeyID string
	StartedAt               time.Time
	OldAmount               int64
	OldBasis                string
}

// ReconcileBillingPeriod is the sole writer of observed subscription cost.
// A locked paid-period row serializes live polling, historical replay and late
// arrivals. Dry runs execute the identical path and roll back the transaction.
func (s *SubscriptionRateStore) ReconcileBillingPeriod(ctx context.Context, periodID int64, asOf time.Time, apply bool) (subscriptioncost.Period, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return subscriptioncost.Period{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	p, err := scanBillingPeriod(tx.QueryRow(ctx, billingPeriodSelect+` WHERE id=$1 FOR UPDATE`, periodID))
	if err != nil {
		return p, err
	}
	// Allow recorder delivery to settle. Later arrivals are included on replay.
	cutoff := asOf.Add(-30 * time.Second)
	rows, err := tx.Query(ctx, `SELECT observed_at,usage_windows FROM provider_balance_snapshots WHERE provider_account_id=$1 AND observed_at>=$2 AND observed_at<$3 AND observed_at<=$4 AND status='success' AND balance_kind='usage' AND COALESCE(normalization_error,'')='' ORDER BY observed_at,id`, p.ProviderAccountID, p.Start, p.End, cutoff)
	if err != nil {
		return p, err
	}
	snapshots := []providerbalance.Snapshot{}
	for rows.Next() {
		snp := providerbalance.Snapshot{Status: "success", BalanceKind: "usage"}
		var raw []byte
		if err = rows.Scan(&snp.ObservedAt, &raw); err != nil {
			rows.Close()
			return p, err
		}
		if err = json.Unmarshal(raw, &snp.UsageWindows); err != nil {
			rows.Close()
			return p, err
		}
		snapshots = append(snapshots, snp)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return p, err
	}
	rows, err = tx.Query(ctx, `SELECT id,completed_at,total_tokens,COALESCE(tenant_id,''),COALESCE(user_id,''),COALESCE(key_id,''),started_at,ROUND(cost_total_cents*1000)::bigint,cost_basis FROM usage_events WHERE provider_account_id=$1 AND completed_at>=$2 AND completed_at<$3 AND completed_at<=$4 ORDER BY completed_at,id FOR UPDATE`, p.ProviderAccountID, p.Start, p.End, cutoff)
	if err != nil {
		return p, err
	}
	events := []allocationEvent{}
	requests := []subscriptioncost.AllocationRequest{}
	for rows.Next() {
		var e allocationEvent
		if err = rows.Scan(&e.ID, &e.CompletedAt, &e.Tokens, &e.TenantID, &e.UserID, &e.KeyID, &e.StartedAt, &e.OldAmount, &e.OldBasis); err != nil {
			rows.Close()
			return p, err
		}
		events = append(events, e)
		requests = append(requests, e.AllocationRequest)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return p, err
	}
	result, err := subscriptioncost.Allocate(p, snapshots, requests)
	if err != nil {
		return p, err
	}
	evidence, _ := json.Marshal(requests)
	h := sha256.New()
	_, _ = h.Write(evidence)
	for _, e := range events {
		_, _ = fmt.Fprintf(h, "%q/%q/%q/%s", e.TenantID, e.UserID, e.KeyID, e.StartedAt.UTC().Format(time.RFC3339Nano))
	}
	evidenceHash := fmt.Sprintf("%x", h.Sum(nil))
	changed := p.EvidenceHash != evidenceHash || p.Allocated != result.Allocated || p.Unattributed != result.Unattributed || p.ConsumedPoints != result.ConsumedPoints || !sameTime(p.ObservedThrough, result.ObservedThrough)
	for _, e := range events {
		a := result.Requests[e.ID]
		if e.OldAmount != a.Amount || e.OldBasis != a.Basis {
			changed = true
		}
	}
	p.Allocated = result.Allocated
	p.Unattributed = result.Unattributed
	p.ConsumedPoints = result.ConsumedPoints
	p.ObservedThrough = result.ObservedThrough
	if !changed {
		return p, nil
	}
	p.Revision++
	p.EvidenceHash = evidenceHash
	if _, err = tx.Exec(ctx, `SELECT set_config('piproxy.subscription_allocator','on',true)`); err != nil {
		return p, err
	}
	for _, e := range events {
		a := result.Requests[e.ID]
		if e.OldAmount == a.Amount && e.OldBasis == a.Basis {
			continue
		}
		if _, err = tx.Exec(ctx, `INSERT INTO subscription_allocation_adjustments(period_id,revision,usage_event_id,old_millicents,new_millicents,old_cost_basis,new_cost_basis,old_cost_cents,old_subscription_rate_card_id) SELECT $1,$2,$3,$4,$5,$6,$7,cost_total_cents,subscription_rate_card_id FROM usage_events WHERE id=$3`, p.ID, p.Revision, e.ID, e.OldAmount, a.Amount, e.OldBasis, a.Basis); err != nil {
			return p, err
		}
		if _, err = tx.Exec(ctx, `UPDATE usage_events SET cost_total_cents=$2::numeric/1000,subscription_rate_card_id=NULL,subscription_billing_period_id=$3,cost_basis=$4 WHERE id=$1`, e.ID, a.Amount, p.ID, a.Basis); err != nil {
			return p, err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE subscription_billing_periods SET allocated_millicents=$2,unattributed_millicents=$3,consumed_points=$4,observed_through=$5,revision=$6,evidence_hash=$7 WHERE id=$1`, p.ID, p.Allocated, p.Unattributed, p.ConsumedPoints, p.ObservedThrough, p.Revision, p.EvidenceHash); err != nil {
		return p, err
	}
	if err = enqueueAllocationComponents(ctx, tx, p, events, result, asOf); err != nil {
		return p, err
	}
	if apply {
		err = tx.Commit(ctx)
	}
	return p, err
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func enqueueAllocationComponents(ctx context.Context, tx pgx.Tx, p subscriptioncost.Period, events []allocationEvent, result subscriptioncost.AllocationResult, now time.Time) error {
	type component struct {
		scope   quota.ScopeType
		id, key string
		limit   quota.LimitType
		amount  int64
	}
	components := map[string]*component{}
	// Retain previous scope membership so a transfer away from the last
	// request in a scope explicitly reconciles its component to zero.
	rows, err := tx.Query(ctx, `SELECT scope_type,scope_id,counter_key,limit_type FROM subscription_quota_components WHERE period_id=$1`, p.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		c := &component{}
		if err = rows.Scan(&c.scope, &c.id, &c.key, &c.limit); err != nil {
			rows.Close()
			return err
		}
		components[c.key] = c
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, e := range events {
		for _, scope := range []struct {
			kind quota.ScopeType
			id   string
		}{{quota.ScopeTenant, e.TenantID}, {quota.ScopeUser, e.UserID}, {quota.ScopeKey, e.KeyID}} {
			if scope.id == "" {
				continue
			}
			for _, window := range []struct {
				period, format string
				limit          quota.LimitType
			}{{"daily", "2006-01-02", quota.LimitDailySpendCents}, {"monthly", "2006-01", quota.LimitMonthlySpendCents}} {
				// Expired counters cannot affect admission; do not resurrect them.
				if e.StartedAt.UTC().Format(window.format) != now.UTC().Format(window.format) {
					continue
				}
				key := fmt.Sprintf("quota:%s:spend_millicents:%s:%s:%s", window.period, scope.kind, scope.id, e.StartedAt.UTC().Format(window.format))
				if components[key] == nil {
					components[key] = &component{scope: scope.kind, id: scope.id, key: key, limit: window.limit}
				}
				components[key].amount += result.Requests[e.ID].Amount
			}
		}
	}
	for _, c := range components {
		// A historical daily/monthly key is no longer an admission counter.
		format := "2006-01-02"
		if c.limit == quota.LimitMonthlySpendCents {
			format = "2006-01"
		}
		if !strings.HasSuffix(c.key, ":"+now.UTC().Format(format)) {
			continue
		}
		componentKey := fmt.Sprintf("subscription-period:%d", p.ID)
		id := fmt.Sprintf("%s:%d:%s", componentKey, p.Revision, c.key)
		if _, err := tx.Exec(ctx, `INSERT INTO quota_spend_settlements(id,scope_type,scope_id,counter_key,limit_type,amount_millicents,operation,component_key,component_version) VALUES($1,$2,$3,$4,$5,$6,'reconcile_component',$7,$8) ON CONFLICT(id) DO NOTHING`, id, c.scope, c.id, c.key, c.limit, c.amount, componentKey, p.Revision); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO subscription_quota_components(period_id,counter_key,scope_type,scope_id,limit_type,amount_millicents) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(period_id,counter_key) DO UPDATE SET amount_millicents=EXCLUDED.amount_millicents`, p.ID, c.key, c.scope, c.id, c.limit, c.amount); err != nil {
			return err
		}
	}
	return nil
}
