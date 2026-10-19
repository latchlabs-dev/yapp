package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Sentinel error returned when a user with the same email already exists
// within the same tenant.
var ErrDuplicateUserEmail = fmt.Errorf("user with this email already exists in this tenant")

// UserRecord represents a user stored in the database.
type UserRecord struct {
	ID          string    `json:"id"`
	TenantID    string    `json:"tenant_id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name,omitempty"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// UserStore provides CRUD access to user storage.
type UserStore struct {
	pool *Pool
}

// NewUserStore creates a new UserStore.
func NewUserStore(pool *Pool) *UserStore {
	return &UserStore{pool: pool}
}

// List returns all users for a tenant.
func (s *UserStore) List(ctx context.Context, tenantID string) ([]*UserRecord, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, tenant_id, email, display_name, status, created_at, updated_at
		 FROM users
		 WHERE tenant_id = $1 AND status <> 'deleted'
		 ORDER BY created_at`,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var result []*UserRecord
	for rows.Next() {
		r := &UserRecord{}
		if err := rows.Scan(&r.ID, &r.TenantID, &r.Email, &r.DisplayName, &r.Status, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// GetByID retrieves a user by ID (scoped to tenant).
func (s *UserStore) GetByID(ctx context.Context, tenantID, userID string) (*UserRecord, error) {
	r := &UserRecord{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, tenant_id, email, display_name, status, created_at, updated_at
		 FROM users
		 WHERE id = $1 AND tenant_id = $2 AND status <> 'deleted'`,
		userID, tenantID,
	).Scan(&r.ID, &r.TenantID, &r.Email, &r.DisplayName, &r.Status, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("get user %s: %w", userID, err)
	}
	return r, nil
}

// GetByEmail retrieves an active user by email within a tenant, returning nil
// when there is none. Used by web-console login to find an existing user
// before provisioning a new one; the email match is case-insensitive because
// identity providers do not normalise the local part consistently.
func (s *UserStore) GetByEmail(ctx context.Context, tenantID, email string) (*UserRecord, error) {
	r := &UserRecord{}
	var displayName *string
	err := s.pool.QueryRow(ctx,
		`SELECT id, tenant_id, email, display_name, status, created_at, updated_at
		 FROM users
		 WHERE tenant_id = $1 AND lower(email) = lower($2) AND status <> 'deleted'`,
		tenantID, email,
	).Scan(&r.ID, &r.TenantID, &r.Email, &displayName, &r.Status, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get user by email: %w", err)
	}
	if displayName != nil {
		r.DisplayName = *displayName
	}
	return r, nil
}

// Insert creates a new user.
func (s *UserStore) Insert(ctx context.Context, record *UserRecord) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, display_name, status, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		record.ID, record.TenantID, record.Email, record.DisplayName, record.Status, record.CreatedAt, record.UpdatedAt,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrDuplicateUserEmail
		}
		return fmt.Errorf("insert user: %w", err)
	}
	return nil
}

// Update modifies an existing user.
func (s *UserStore) Update(ctx context.Context, record *UserRecord) error {
	record.UpdatedAt = time.Now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("update user begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	_, err = tx.Exec(ctx,
		`UPDATE users
		 SET email = $3, display_name = $4, status = $5, updated_at = $6
		 WHERE id = $1 AND tenant_id = $2 AND status <> 'deleted'`,
		record.ID, record.TenantID, record.Email, record.DisplayName, record.Status, record.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("update user: %w", err)
	}
	if record.Status != "active" {
		// Updating the user first serializes this with session minting, so a
		// concurrent login cannot insert an unrevoked session after this sweep.
		if _, err := tx.Exec(ctx, `UPDATE web_sessions SET revoked_at = now() WHERE tenant_id = $1 AND user_id = $2 AND revoked_at IS NULL`, record.TenantID, record.ID); err != nil {
			return fmt.Errorf("update user revoke web sessions: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("update user commit: %w", err)
	}
	return nil
}

// Delete archives a user by ID (scoped to tenant) and revokes their API keys.
func (s *UserStore) Delete(ctx context.Context, tenantID, userID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err := tx.Exec(ctx, `
		UPDATE api_keys
		SET status = 'revoked', revoked_at = COALESCE(revoked_at, NOW())
		WHERE tenant_id = $1 AND user_id = $2 AND status <> 'revoked'
	`, tenantID, userID); err != nil {
		return fmt.Errorf("delete user revoke keys: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE users
		SET status = 'deleted', updated_at = NOW()
		WHERE id = $1 AND tenant_id = $2 AND status <> 'deleted'
	`, userID, tenantID); err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE web_sessions SET revoked_at = now() WHERE tenant_id = $1 AND user_id = $2 AND revoked_at IS NULL`, tenantID, userID); err != nil {
		return fmt.Errorf("delete user revoke web sessions: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("delete user commit: %w", err)
	}
	return nil
}
