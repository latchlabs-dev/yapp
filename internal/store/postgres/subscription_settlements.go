package postgres

import (
	"context"
	"time"

	"github.com/earlgrey/piproxy/internal/quota"
)

// FlushSubscriptionComponents also runs while Codex quota enforcement is
// disabled, keeping reporting and later quota re-enablement consistent.
func (s *SubscriptionRateStore) FlushSubscriptionComponents(ctx context.Context, counter quota.SpendComponentCounterStore, now time.Time) error {
	if counter == nil {
		return nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id,counter_key,component_key,component_version,amount_millicents,limit_type FROM quota_spend_settlements WHERE operation='reconcile_component' ORDER BY created_at,id LIMIT 1000`)
	if err != nil {
		return err
	}
	var settlements []quota.SpendSettlement
	for rows.Next() {
		var x quota.SpendSettlement
		if err = rows.Scan(&x.ID, &x.CounterKey, &x.ComponentKey, &x.ComponentVersion, &x.AmountMillicents, &x.Limit); err != nil {
			rows.Close()
			return err
		}
		settlements = append(settlements, x)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, x := range settlements {
		end := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day()+1, 0, 0, 0, 0, time.UTC)
		if x.Limit == quota.LimitMonthlySpendCents {
			end = time.Date(now.UTC().Year(), now.UTC().Month()+1, 1, 0, 0, 0, 0, time.UTC)
		}
		if _, err = counter.ReconcileSpendComponent(ctx, x.CounterKey, x.ComponentKey, x.ComponentVersion, x.AmountMillicents, end.Sub(now)); err != nil {
			return err
		}
		if _, err = s.pool.Exec(ctx, `DELETE FROM quota_spend_settlements WHERE id=$1`, x.ID); err != nil {
			return err
		}
	}
	return nil
}
