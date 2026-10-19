package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/earlgrey/piproxy/internal/auth"
)

// KeyRecord represents a stored API key.
type KeyRecord struct {
	ID        string     `json:"id"`
	TenantID  string     `json:"tenant_id"`
	UserID    string     `json:"user_id"`
	Prefix    string     `json:"prefix"`
	KeyHash   string     `json:"-"`
	Scopes    []string   `json:"scopes"`
	Status    string     `json:"status"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// KeyStore provides access to API key storage.
type KeyStore struct {
	pool *Pool
}

// NewKeyStore creates a new KeyStore.
func NewKeyStore(pool *Pool) *KeyStore {
	return &KeyStore{pool: pool}
}

// GetByID looks up an API key by its ID.
func (s *KeyStore) GetByID(ctx context.Context, id string) (*KeyRecord, error) {
	query := `
		SELECT id, tenant_id, user_id, prefix, key_hash, scopes, status, expires_at, revoked_at, created_at
		FROM api_keys
		WHERE id = $1
	`
	row := s.pool.QueryRow(ctx, query, id)

	record := &KeyRecord{}
	var keyHash []byte
	err := row.Scan(
		&record.ID,
		&record.TenantID,
		&record.UserID,
		&record.Prefix,
		&keyHash,
		&record.Scopes,
		&record.Status,
		&record.ExpiresAt,
		&record.RevokedAt,
		&record.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("get key by id: %w", err)
	}
	record.KeyHash = string(keyHash)
	return record, nil
}

// GetByPrefix looks up an API key by its prefix.
func (s *KeyStore) GetByPrefix(ctx context.Context, prefix string) (*KeyRecord, error) {
	query := `
		SELECT id, tenant_id, user_id, prefix, key_hash, scopes, status, expires_at, revoked_at, created_at
		FROM api_keys
		WHERE prefix = $1
	`
	row := s.pool.QueryRow(ctx, query, prefix)

	record := &KeyRecord{}
	var keyHash []byte
	err := row.Scan(
		&record.ID,
		&record.TenantID,
		&record.UserID,
		&record.Prefix,
		&keyHash,
		&record.Scopes,
		&record.Status,
		&record.ExpiresAt,
		&record.RevokedAt,
		&record.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("get key by prefix: %w", err)
	}
	record.KeyHash = string(keyHash)
	return record, nil
}

// Insert creates a new API key record.
func (s *KeyStore) Insert(ctx context.Context, record *KeyRecord) error {
	record.Scopes = auth.BaselineKeyScopes(record.Scopes)
	query := `
		INSERT INTO api_keys (id, tenant_id, user_id, prefix, key_hash, scopes, status, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`
	_, err := s.pool.Exec(ctx, query,
		record.ID,
		record.TenantID,
		record.UserID,
		record.Prefix,
		[]byte(record.KeyHash),
		record.Scopes,
		record.Status,
		record.ExpiresAt,
		record.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert key: %w", err)
	}
	return nil
}

// Revoke sets a key's status to revoked.
func (s *KeyStore) Revoke(ctx context.Context, keyID string) error {
	query := `UPDATE api_keys SET status = 'revoked', revoked_at = NOW() WHERE id = $1`
	_, err := s.pool.Exec(ctx, query, keyID)
	if err != nil {
		return fmt.Errorf("revoke key: %w", err)
	}
	return nil
}

// List returns all API keys for a tenant, optionally filtered by user.
func (s *KeyStore) List(ctx context.Context, tenantID, userID string) ([]*KeyRecord, error) {
	var (
		rows interface {
			Close()
			Next() bool
			Scan(...interface{}) error
			Err() error
		}
		err error
	)

	if userID != "" {
		rows, err = s.pool.Query(ctx,
			`SELECT id, tenant_id, user_id, prefix, scopes, status, expires_at, revoked_at, created_at
			 FROM api_keys WHERE tenant_id = $1 AND user_id = $2 ORDER BY created_at DESC`,
			tenantID, userID,
		)
	} else {
		rows, err = s.pool.Query(ctx,
			`SELECT id, tenant_id, user_id, prefix, scopes, status, expires_at, revoked_at, created_at
			 FROM api_keys WHERE tenant_id = $1 ORDER BY created_at DESC`,
			tenantID,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	defer rows.Close()

	var result []*KeyRecord
	for rows.Next() {
		r := &KeyRecord{}
		if err := rows.Scan(&r.ID, &r.TenantID, &r.UserID, &r.Prefix, &r.Scopes, &r.Status, &r.ExpiresAt, &r.RevokedAt, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan key: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// UpdateLastUsed updates the last_used_at timestamp for a key.
func (s *KeyStore) UpdateLastUsed(ctx context.Context, keyID string) error {
	query := `UPDATE api_keys SET last_used_at = NOW() WHERE id = $1`
	_, err := s.pool.Exec(ctx, query, keyID)
	if err != nil {
		return fmt.Errorf("update last used: %w", err)
	}
	return nil
}
