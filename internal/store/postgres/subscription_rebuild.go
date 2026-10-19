package postgres

import (
	"context"
	"fmt"
	"time"
)

type SubscriptionCounterBaseline struct {
	CounterKey string           `json:"counter_key"`
	Amount     int64            `json:"amount_millicents"`
	ExpiresAt  time.Time        `json:"expires_at"`
	Components map[string]int64 `json:"components"`
}

// SubscriptionCounterBaselines is used only during a historical maintenance
// window with proxy/admin writers stopped and recorder queues drained. It
// reconstructs affected active counters from ALL providers, not just Codex.
func (s *SubscriptionRateStore) SubscriptionCounterBaselines(ctx context.Context, provider string, now time.Time) ([]SubscriptionCounterBaseline, error) {
	now = now.UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT COALESCE(tenant_id,''),COALESCE(user_id,''),COALESCE(key_id,'') FROM usage_events WHERE provider_account_id=$1 AND started_at>=$2`, provider, month)
	if err != nil {
		return nil, err
	}
	scopes := map[string]string{}
	for rows.Next() {
		var tenant, user, key string
		if err = rows.Scan(&tenant, &user, &key); err != nil {
			rows.Close()
			return nil, err
		}
		for _, x := range []struct{ kind, id string }{{"tenant", tenant}, {"user", user}, {"api_key", key}} {
			if x.id != "" {
				scopes[x.kind+":"+x.id] = x.kind
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	baselines := []SubscriptionCounterBaseline{}
	for scope, kind := range scopes {
		id := scope[len(kind)+1:]
		column := map[string]string{"tenant": "tenant_id", "user": "user_id", "api_key": "key_id"}[kind]
		for _, w := range []struct {
			name, format string
			from, to     time.Time
		}{{"daily", "2006-01-02", day, day.AddDate(0, 0, 1)}, {"monthly", "2006-01", month, month.AddDate(0, 1, 0)}} {
			b := SubscriptionCounterBaseline{CounterKey: fmt.Sprintf("quota:%s:spend_millicents:%s:%s", w.name, scope, w.from.Format(w.format)), ExpiresAt: w.to, Components: map[string]int64{}}
			query := fmt.Sprintf(`SELECT ROUND(COALESCE(SUM(cost_total_cents),0)*1000)::bigint FROM usage_events WHERE %s=$1 AND started_at>=$2 AND started_at<$3 AND completed_at IS NOT NULL`, column)
			if err = s.pool.QueryRow(ctx, query, id, w.from, w.to).Scan(&b.Amount); err != nil {
				return nil, err
			}
			query = fmt.Sprintf(`SELECT p.id,p.revision,ROUND(SUM(e.cost_total_cents)*1000)::bigint FROM usage_events e JOIN subscription_billing_periods p ON p.id=e.subscription_billing_period_id WHERE e.%s=$1 AND e.started_at>=$2 AND e.started_at<$3 GROUP BY p.id,p.revision`, column)
			rows, err = s.pool.Query(ctx, query, id, w.from, w.to)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var period, revision, amount int64
				if err = rows.Scan(&period, &revision, &amount); err != nil {
					rows.Close()
					return nil, err
				}
				prefix := fmt.Sprintf("subscription-period:%d", period)
				b.Components[prefix+":version"] = revision
				b.Components[prefix+":amount"] = amount
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return nil, err
			}
			baselines = append(baselines, b)
		}
	}
	return baselines, nil
}

func (s *SubscriptionRateStore) ClearRebuiltCounterSettlements(ctx context.Context, counter string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM quota_spend_settlements WHERE counter_key=$1`, counter)
	return err
}
