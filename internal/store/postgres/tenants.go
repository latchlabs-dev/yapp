package postgres

import (
	"context"
	"fmt"
	"time"
)

// TenantRecord represents a tenant stored in the database.
type TenantRecord struct {
	ID                         string    `json:"id"`
	Name                       string    `json:"name"`
	Status                     string    `json:"status"`
	SessionDistillationEnabled bool      `json:"session_distillation_enabled"`
	CreatedAt                  time.Time `json:"created_at"`
	UpdatedAt                  time.Time `json:"updated_at"`
}

// TenantStore provides CRUD access to tenant storage.
type TenantStore struct {
	pool *Pool
}

// NewTenantStore creates a new TenantStore.
func NewTenantStore(pool *Pool) *TenantStore {
	return &TenantStore{pool: pool}
}

// List returns all tenants.
func (s *TenantStore) List(ctx context.Context) ([]*TenantRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, status, session_distillation_enabled, created_at, updated_at
		FROM tenants
		WHERE status <> 'deleted'
		ORDER BY created_at
	`)
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	defer rows.Close()

	var result []*TenantRecord
	for rows.Next() {
		r := &TenantRecord{}
		if err := rows.Scan(&r.ID, &r.Name, &r.Status, &r.SessionDistillationEnabled, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan tenant: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// GetByID retrieves a tenant by ID.
func (s *TenantStore) GetByID(ctx context.Context, id string) (*TenantRecord, error) {
	r := &TenantRecord{}
	err := s.pool.QueryRow(ctx, `
		SELECT id, name, status, session_distillation_enabled, created_at, updated_at
		FROM tenants
		WHERE id = $1 AND status <> 'deleted'
	`, id).
		Scan(&r.ID, &r.Name, &r.Status, &r.SessionDistillationEnabled, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("get tenant %s: %w", id, err)
	}
	return r, nil
}

// Insert creates a new tenant.
func (s *TenantStore) Insert(ctx context.Context, record *TenantRecord) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO tenants (id, name, status, session_distillation_enabled, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6)`,
		record.ID, record.Name, record.Status, record.SessionDistillationEnabled, record.CreatedAt, record.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert tenant: %w", err)
	}
	return nil
}

// Update modifies an existing tenant.
func (s *TenantStore) Update(ctx context.Context, record *TenantRecord) error {
	record.UpdatedAt = time.Now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("update tenant begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	_, err = tx.Exec(ctx,
		`UPDATE tenants
		 SET name = $2, status = $3, session_distillation_enabled = $4, updated_at = $5
		 WHERE id = $1 AND status <> 'deleted'`,
		record.ID, record.Name, record.Status, record.SessionDistillationEnabled, record.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("update tenant: %w", err)
	}
	if record.Status != "active" {
		if _, err := tx.Exec(ctx, `UPDATE web_sessions SET revoked_at = now() WHERE tenant_id = $1 AND revoked_at IS NULL`, record.ID); err != nil {
			return fmt.Errorf("update tenant revoke web sessions: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("update tenant commit: %w", err)
	}
	return nil
}

// Delete archives a tenant, its users, and any tenant-scoped API keys.
func (s *TenantStore) Delete(ctx context.Context, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("delete tenant: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Lock the tenant before its users, matching the session-mint lock order.
	// Otherwise archival could miss a session created concurrently with it.
	if _, err := tx.Exec(ctx, `SELECT id FROM tenants WHERE id = $1 FOR UPDATE`, id); err != nil {
		return fmt.Errorf("delete tenant lock: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE api_keys
		SET status = 'revoked', revoked_at = COALESCE(revoked_at, NOW())
		WHERE tenant_id = $1 AND status <> 'revoked'
	`, id); err != nil {
		return fmt.Errorf("delete tenant revoke keys: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE users
		SET status = 'deleted', updated_at = NOW()
		WHERE tenant_id = $1 AND status <> 'deleted'
	`, id); err != nil {
		return fmt.Errorf("delete tenant users: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE tenants
		SET status = 'deleted', updated_at = NOW()
		WHERE id = $1 AND status <> 'deleted'
	`, id); err != nil {
		return fmt.Errorf("delete tenant: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE web_sessions SET revoked_at = now() WHERE tenant_id = $1 AND revoked_at IS NULL`, id); err != nil {
		return fmt.Errorf("delete tenant revoke web sessions: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("delete tenant commit: %w", err)
	}
	return nil
}
