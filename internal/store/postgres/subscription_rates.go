package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/earlgrey/piproxy/internal/providerbalance"
	"github.com/earlgrey/piproxy/internal/quota"
	"github.com/earlgrey/piproxy/internal/subscriptioncost"
)

// SubscriptionRateStore persists and reads daily effective subscription rates.
type SubscriptionRateStore struct {
	pool *Pool
}

func NewSubscriptionRateStore(pool *Pool) *SubscriptionRateStore {
	return &SubscriptionRateStore{pool: pool}
}

func (s *SubscriptionRateStore) ListSubscriptionAccounts(ctx context.Context) ([]subscriptioncost.Account, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id,
		       COALESCE(config->'billing'->>'currency', 'USD'),
		       CASE WHEN jsonb_typeof(config->'billing'->'monthly_cost_cents') = 'number'
		            THEN (config->'billing'->>'monthly_cost_cents')::double precision ELSE 0 END,
		       CASE WHEN jsonb_typeof(config->'billing'->'fallback_usd_per_million_tokens') = 'number'
		            THEN (config->'billing'->>'fallback_usd_per_million_tokens')::double precision ELSE 0 END
		FROM provider_accounts
		WHERE status = 'active'
		  AND config->'billing'->>'mode' = 'subscription'
		  AND COALESCE(config->'billing'->>'allocation_method','') <> 'observed_delta'
		  AND CASE WHEN jsonb_typeof(config->'billing'->'monthly_cost_cents') = 'number'
		           THEN (config->'billing'->>'monthly_cost_cents')::numeric ELSE 0 END > 0
		ORDER BY id
	`)
	if err != nil {
		return nil, fmt.Errorf("list subscription accounts: %w", err)
	}
	defer rows.Close()
	accounts := []subscriptioncost.Account{}
	for rows.Next() {
		var account subscriptioncost.Account
		if err := rows.Scan(&account.ID, &account.Currency, &account.MonthlyCostCents, &account.FallbackUSDPerMillion); err != nil {
			return nil, fmt.Errorf("scan subscription account: %w", err)
		}
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

func (s *SubscriptionRateStore) ListUsage(ctx context.Context, providerAccountID string, from, to time.Time) ([]subscriptioncost.Usage, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT COALESCE(NULLIF(upstream_model, ''), NULLIF(model, ''), 'unknown') AS model,
		       COALESCE(SUM(total_tokens), 0)::bigint AS total_tokens
		FROM usage_events
		WHERE provider_account_id = $1
		  AND started_at >= $2 AND started_at < $3
		  AND completed_at IS NOT NULL AND completed_at < $3
		GROUP BY 1
		ORDER BY 1
	`, providerAccountID, from, to)
	if err != nil {
		return nil, fmt.Errorf("list subscription usage: %w", err)
	}
	defer rows.Close()
	usageRows := []subscriptioncost.Usage{}
	for rows.Next() {
		var row subscriptioncost.Usage
		if err := rows.Scan(&row.Model, &row.Tokens); err != nil {
			return nil, fmt.Errorf("scan subscription usage: %w", err)
		}
		usageRows = append(usageRows, row)
	}
	return usageRows, rows.Err()
}

func (s *SubscriptionRateStore) ListUtilizationSnapshots(ctx context.Context, providerAccountID string, from, to time.Time) ([]providerbalance.Snapshot, error) {
	rows, err := s.pool.Query(ctx, `
		(
			SELECT id, observed_at, usage_windows
			FROM provider_balance_snapshots
			WHERE provider_account_id = $1
			  AND observed_at <= $2
			  AND observed_at >= $2 - interval '15 minutes'
			  AND status = 'success'
			  AND balance_kind = 'usage'
			  AND COALESCE(normalization_error, '') = ''
			ORDER BY observed_at DESC, id DESC
			LIMIT 1
		)
		UNION ALL
		(
			SELECT id, observed_at, usage_windows
			FROM provider_balance_snapshots
			WHERE provider_account_id = $1
			  AND observed_at > $2 AND observed_at <= $3
			  AND status = 'success'
			  AND balance_kind = 'usage'
			  AND COALESCE(normalization_error, '') = ''
		)
		ORDER BY observed_at, id
	`, providerAccountID, from, to)
	if err != nil {
		return nil, fmt.Errorf("list subscription utilization snapshots: %w", err)
	}
	defer rows.Close()
	snapshots := []providerbalance.Snapshot{}
	for rows.Next() {
		var snapshot providerbalance.Snapshot
		var windows []byte
		if err := rows.Scan(&snapshot.ID, &snapshot.ObservedAt, &windows); err != nil {
			return nil, fmt.Errorf("scan subscription utilization snapshot: %w", err)
		}
		if err := json.Unmarshal(windows, &snapshot.UsageWindows); err != nil {
			return nil, fmt.Errorf("decode subscription utilization windows: %w", err)
		}
		snapshot.Status = "success"
		snapshot.BalanceKind = "usage"
		snapshots = append(snapshots, snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list subscription utilization snapshot rows: %w", err)
	}
	return snapshots, nil
}

func (s *SubscriptionRateStore) LatestFallback(ctx context.Context, providerAccountID string, before time.Time) (*subscriptioncost.RateCard, error) {
	row := s.pool.QueryRow(ctx, rateCardSelect+`
		WHERE provider_account_id = $1 AND model = '*' AND rate_date < $2
		ORDER BY rate_date DESC, calculated_at DESC
		LIMIT 1
	`, providerAccountID, before)
	card, err := scanRateCard(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("get previous subscription rate: %w", err)
	}
	return card, nil
}

func (s *SubscriptionRateStore) UpsertRateCards(ctx context.Context, cards []subscriptioncost.RateCard) error {
	for i := range cards {
		card := &cards[i]
		err := s.pool.QueryRow(ctx, `
			INSERT INTO provider_subscription_rate_cards (
				provider_account_id, rate_date, model, currency, monthly_cost_cents,
				accrued_cost_cents, source_period_start, source_period_end, source_tokens,
				source_usage_percent, source_usage_window, source_usage_window_seconds,
				source_usage_window_resets_at,
				usd_per_million_tokens, calculation_method, calculated_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,now())
			ON CONFLICT (provider_account_id, rate_date, model) DO UPDATE SET
				currency = EXCLUDED.currency,
				monthly_cost_cents = EXCLUDED.monthly_cost_cents,
				accrued_cost_cents = EXCLUDED.accrued_cost_cents,
				source_period_start = EXCLUDED.source_period_start,
				source_period_end = EXCLUDED.source_period_end,
				source_tokens = EXCLUDED.source_tokens,
				source_usage_percent = EXCLUDED.source_usage_percent,
				source_usage_window = EXCLUDED.source_usage_window,
				source_usage_window_seconds = EXCLUDED.source_usage_window_seconds,
				source_usage_window_resets_at = EXCLUDED.source_usage_window_resets_at,
				usd_per_million_tokens = EXCLUDED.usd_per_million_tokens,
				calculation_method = EXCLUDED.calculation_method,
				calculated_at = now()
			RETURNING id, calculated_at
		`, card.ProviderAccountID, card.RateDate, card.Model, card.Currency,
			card.MonthlyCostCents, card.AccruedCostCents, card.SourcePeriodStart,
			card.SourcePeriodEnd, card.SourceTokens, card.SourceUsagePercent,
			card.SourceUsageWindow, card.SourceUsageWindowSeconds, card.SourceUsageWindowResetsAt,
			card.USDPerMillionTokens, card.CalculationMethod).Scan(&card.ID, &card.CalculatedAt)
		if err != nil {
			return fmt.Errorf("upsert subscription rate card: %w", err)
		}
	}
	return nil
}

// AllocateRateCard applies a utilization-derived rate to the usage events that
// formed its denominator. It only increases recorded cost: recalculating a card
// is therefore idempotent and can never silently refund quota because a later
// provider observation corrected its percentage downward. It also records an
// absolute Postgres-ledger floor for each affected quota counter. Raising Redis
// to that floor is safe whether the original per-request settlement succeeded,
// failed, or returned an ambiguous network result.
func (s *SubscriptionRateStore) AllocateRateCard(ctx context.Context, card subscriptioncost.RateCard) error {
	if card.ID <= 0 || card.ProviderAccountID == "" || card.USDPerMillionTokens <= 0 ||
		!card.SourcePeriodEnd.After(card.SourcePeriodStart) {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin subscription allocation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var observed bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE(config->'billing'->>'allocation_method','')='observed_delta' FROM provider_accounts WHERE id=$1`, card.ProviderAccountID).Scan(&observed); err != nil {
		return err
	}
	if observed {
		return nil
	}

	// Recalculation can legitimately omit a model that appeared in an older
	// card for the same UTC date. Keep only model rows derived from this exact
	// fallback interval and method; otherwise the resolver could continue to
	// serve a stale per-model price instead of the new account fallback.
	if card.Model == subscriptioncost.AccountFallbackModel {
		if _, err := tx.Exec(ctx, `
			DELETE FROM provider_subscription_rate_cards
			WHERE provider_account_id = $1
			  AND rate_date = $2
			  AND model <> '*'
			  AND (
				source_period_start IS DISTINCT FROM $3 OR
				source_period_end IS DISTINCT FROM $4 OR
				calculation_method IS DISTINCT FROM $5
			  )
		`, card.ProviderAccountID, card.RateDate, card.SourcePeriodStart, card.SourcePeriodEnd, card.CalculationMethod); err != nil {
			return fmt.Errorf("delete stale subscription model rate cards: %w", err)
		}
	}

	type usageAllocation struct {
		id          string
		tenantID    *string
		userID      *string
		keyID       *string
		totalTokens int64
		costCents   float64
		startedAt   time.Time
	}
	rows, err := tx.Query(ctx, `
		SELECT id, tenant_id, user_id, key_id, total_tokens,
		       cost_total_cents::double precision, started_at
		FROM usage_events
		WHERE provider_account_id = $1
		  AND started_at >= $2 AND started_at < $3
		  AND completed_at IS NOT NULL AND completed_at < $3
		  AND total_tokens > 0
		ORDER BY started_at, id
		FOR UPDATE
	`, card.ProviderAccountID, card.SourcePeriodStart, card.SourcePeriodEnd)
	if err != nil {
		return fmt.Errorf("list usage for subscription allocation: %w", err)
	}
	allocations := []usageAllocation{}
	for rows.Next() {
		var allocation usageAllocation
		if err := rows.Scan(
			&allocation.id, &allocation.tenantID, &allocation.userID, &allocation.keyID,
			&allocation.totalTokens, &allocation.costCents, &allocation.startedAt,
		); err != nil {
			rows.Close()
			return fmt.Errorf("scan usage for subscription allocation: %w", err)
		}
		allocations = append(allocations, allocation)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("list usage for subscription allocation rows: %w", err)
	}
	rows.Close()

	type spendFloor struct {
		scopeType  quota.ScopeType
		scopeID    string
		limit      quota.LimitType
		counterKey string
		from       time.Time
		to         time.Time
	}
	spendFloors := make(map[string]spendFloor)
	for _, allocation := range allocations {
		expectedCostCents := float64(allocation.totalTokens) / 1_000_000 * card.USDPerMillionTokens * 100
		if expectedCostCents > allocation.costCents {
			if _, err := tx.Exec(ctx, `
				UPDATE usage_events
				SET cost_total_cents = $2,
				    subscription_rate_card_id = $3,
				    cost_basis = 'subscription_rate_card'
				WHERE id = $1
			`, allocation.id, expectedCostCents, card.ID); err != nil {
				return fmt.Errorf("update subscription allocation for usage event %s: %w", allocation.id, err)
			}
		}

		scopes := []struct {
			scopeType quota.ScopeType
			scopeID   *string
		}{
			{scopeType: quota.ScopeKey, scopeID: allocation.keyID},
			{scopeType: quota.ScopeUser, scopeID: allocation.userID},
			{scopeType: quota.ScopeTenant, scopeID: allocation.tenantID},
		}
		for _, scope := range scopes {
			if scope.scopeID == nil || *scope.scopeID == "" {
				continue
			}
			startedAt := allocation.startedAt.UTC()
			dayStart := time.Date(startedAt.Year(), startedAt.Month(), startedAt.Day(), 0, 0, 0, 0, time.UTC)
			monthStart := time.Date(startedAt.Year(), startedAt.Month(), 1, 0, 0, 0, 0, time.UTC)
			dailyKey := fmt.Sprintf("quota:daily:spend_millicents:%s:%s:%s", scope.scopeType, *scope.scopeID, dayStart.Format("2006-01-02"))
			monthlyKey := fmt.Sprintf("quota:monthly:spend_millicents:%s:%s:%s", scope.scopeType, *scope.scopeID, monthStart.Format("2006-01"))
			spendFloors[dailyKey] = spendFloor{scopeType: scope.scopeType, scopeID: *scope.scopeID, limit: quota.LimitDailySpendCents, counterKey: dailyKey, from: dayStart, to: dayStart.AddDate(0, 0, 1)}
			spendFloors[monthlyKey] = spendFloor{scopeType: scope.scopeType, scopeID: *scope.scopeID, limit: quota.LimitMonthlySpendCents, counterKey: monthlyKey, from: monthStart, to: monthStart.AddDate(0, 1, 0)}
		}
	}

	for _, floor := range spendFloors {
		targetMillicents, err := subscriptionSpendFloor(ctx, tx, floor.scopeType, floor.scopeID, floor.from, floor.to, card.SourcePeriodEnd)
		if err != nil {
			return err
		}
		if targetMillicents <= 0 {
			continue
		}
		settlementID := fmt.Sprintf("subscription-floor:%d:%s:%d", card.ID, floor.counterKey, targetMillicents)
		if _, err := tx.Exec(ctx, `
			INSERT INTO quota_spend_settlements (
				id, scope_type, scope_id, counter_key, limit_type, amount_millicents, operation
			) VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (id) DO NOTHING
		`, settlementID, floor.scopeType, floor.scopeID, floor.counterKey, floor.limit,
			targetMillicents, quota.SpendSettlementEnsureMinimum); err != nil {
			return fmt.Errorf("enqueue subscription spend floor %s: %w", settlementID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit subscription allocation: %w", err)
	}
	return nil
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func subscriptionSpendFloor(ctx context.Context, queryer queryRower, scopeType quota.ScopeType, scopeID string, from, to, completedBefore time.Time) (int64, error) {
	var predicate string
	switch scopeType {
	case quota.ScopeKey:
		predicate = "key_id = $1"
	case quota.ScopeUser:
		predicate = "user_id = $1"
	case quota.ScopeTenant:
		predicate = "tenant_id = $1"
	default:
		return 0, fmt.Errorf("unsupported subscription spend-floor scope %s", scopeType)
	}
	query := fmt.Sprintf(`
		SELECT ROUND(COALESCE(SUM(cost_total_cents), 0) * %d)::bigint
		FROM usage_events
		WHERE %s
		  AND cost_basis NOT IN ('subscription_observed_delta','subscription_pending')
		  AND started_at >= $2 AND started_at < $3
		  AND completed_at IS NOT NULL AND completed_at < $4
	`, quota.SpendCounterScale, predicate)
	var target int64
	if err := queryer.QueryRow(ctx, query, scopeID, from, to, completedBefore).Scan(&target); err != nil {
		return 0, fmt.Errorf("calculate subscription spend floor for %s %s: %w", scopeType, scopeID, err)
	}
	return target, nil
}

func (s *SubscriptionRateStore) ListRateCards(ctx context.Context, providerAccountID string, rateDate time.Time) ([]subscriptioncost.RateCard, error) {
	rows, err := s.pool.Query(ctx, rateCardSelect+`
		WHERE provider_account_id = $1
		  AND rate_date = (
			SELECT MAX(rate_date)
			FROM provider_subscription_rate_cards
			WHERE provider_account_id = $1 AND rate_date <= $2::date
		  )
		ORDER BY CASE WHEN model = '*' THEN 0 ELSE 1 END, model
	`, providerAccountID, rateDate)
	if err != nil {
		return nil, fmt.Errorf("list subscription rate cards: %w", err)
	}
	return scanRateCards(rows)
}

func (s *SubscriptionRateStore) ListActiveRateCards(ctx context.Context, rateDate time.Time) ([]subscriptioncost.RateCard, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT rc.id, rc.provider_account_id, rc.rate_date, rc.model, rc.currency,
		       rc.monthly_cost_cents::double precision, rc.accrued_cost_cents::double precision,
		       rc.source_period_start, rc.source_period_end, rc.source_tokens,
		       rc.source_usage_percent::double precision, COALESCE(rc.source_usage_window, ''),
		       rc.source_usage_window_seconds, rc.source_usage_window_resets_at,
		       rc.usd_per_million_tokens::double precision, rc.calculation_method, rc.calculated_at
		FROM provider_subscription_rate_cards rc
		JOIN (
			SELECT provider_account_id, MAX(rate_date) AS rate_date
			FROM provider_subscription_rate_cards
			WHERE rate_date <= $1::date
			GROUP BY provider_account_id
		) latest USING (provider_account_id, rate_date)
		ORDER BY rc.provider_account_id, CASE WHEN rc.model = '*' THEN 0 ELSE 1 END, rc.model
	`, rateDate)
	if err != nil {
		return nil, fmt.Errorf("list active subscription rate cards: %w", err)
	}
	return scanRateCards(rows)
}

const rateCardSelect = `
	SELECT id, provider_account_id, rate_date, model, currency,
	       monthly_cost_cents::double precision, accrued_cost_cents::double precision,
	       source_period_start, source_period_end, source_tokens,
	       source_usage_percent::double precision, COALESCE(source_usage_window, ''),
	       source_usage_window_seconds, source_usage_window_resets_at,
	       usd_per_million_tokens::double precision, calculation_method, calculated_at
	FROM provider_subscription_rate_cards
`

type rateRowScanner interface {
	Scan(dest ...any) error
}

func scanRateCard(row rateRowScanner) (*subscriptioncost.RateCard, error) {
	card := &subscriptioncost.RateCard{}
	err := row.Scan(
		&card.ID, &card.ProviderAccountID, &card.RateDate, &card.Model, &card.Currency,
		&card.MonthlyCostCents, &card.AccruedCostCents, &card.SourcePeriodStart,
		&card.SourcePeriodEnd, &card.SourceTokens, &card.SourceUsagePercent,
		&card.SourceUsageWindow, &card.SourceUsageWindowSeconds, &card.SourceUsageWindowResetsAt,
		&card.USDPerMillionTokens,
		&card.CalculationMethod, &card.CalculatedAt,
	)
	return card, err
}

type rowsScanner interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close()
}

func scanRateCards(rows rowsScanner) ([]subscriptioncost.RateCard, error) {
	defer rows.Close()
	cards := []subscriptioncost.RateCard{}
	for rows.Next() {
		card, err := scanRateCard(rows)
		if err != nil {
			return nil, fmt.Errorf("scan subscription rate card: %w", err)
		}
		cards = append(cards, *card)
	}
	return cards, rows.Err()
}
