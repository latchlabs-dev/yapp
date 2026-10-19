package postgres

import (
	"context"
	"fmt"

	"github.com/earlgrey/piproxy/internal/quota"
)

// QuotaSpendSettlementStore is the Postgres outbox for exact subscription
// charges awaiting idempotent application to Redis.
type QuotaSpendSettlementStore struct {
	pool *Pool
}

func NewQuotaSpendSettlementStore(pool *Pool) *QuotaSpendSettlementStore {
	return &QuotaSpendSettlementStore{pool: pool}
}

func (s *QuotaSpendSettlementStore) EnqueueSpendSettlements(ctx context.Context, settlements []quota.SpendSettlement) error {
	if len(settlements) == 0 {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin spend settlement transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, settlement := range settlements {
		operation := settlement.Operation
		if operation == "" {
			operation = quota.SpendSettlementAdd
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO quota_spend_settlements (
				id, scope_type, scope_id, counter_key, limit_type, amount_millicents, operation, component_key, component_version
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			ON CONFLICT (id) DO NOTHING
		`, settlement.ID, settlement.ScopeType, settlement.ScopeID, settlement.CounterKey,
			settlement.Limit, settlement.AmountMillicents, operation, settlement.ComponentKey, settlement.ComponentVersion)
		if err != nil {
			return fmt.Errorf("insert spend settlement %s: %w", settlement.ID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit spend settlements: %w", err)
	}
	return nil
}

func (s *QuotaSpendSettlementStore) ListPendingSpendSettlements(ctx context.Context, scopeType quota.ScopeType, scopeID string, limit int) ([]quota.SpendSettlement, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, scope_type, scope_id, counter_key, limit_type, amount_millicents, operation, component_key, component_version
		FROM quota_spend_settlements
		WHERE scope_type = $1 AND scope_id = $2
		ORDER BY created_at, id
		LIMIT $3
	`, scopeType, scopeID, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending spend settlements: %w", err)
	}
	defer rows.Close()
	settlements := make([]quota.SpendSettlement, 0)
	for rows.Next() {
		var settlement quota.SpendSettlement
		if err := rows.Scan(
			&settlement.ID,
			&settlement.ScopeType,
			&settlement.ScopeID,
			&settlement.CounterKey,
			&settlement.Limit,
			&settlement.AmountMillicents,
			&settlement.Operation,
			&settlement.ComponentKey,
			&settlement.ComponentVersion,
		); err != nil {
			return nil, fmt.Errorf("scan pending spend settlement: %w", err)
		}
		settlements = append(settlements, settlement)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pending spend settlement rows: %w", err)
	}
	return settlements, nil
}

func (s *QuotaSpendSettlementStore) DeleteSpendSettlement(ctx context.Context, id string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM quota_spend_settlements WHERE id = $1`, id); err != nil {
		return fmt.Errorf("delete spend settlement %s: %w", id, err)
	}
	return nil
}
