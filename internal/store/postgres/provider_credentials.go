package postgres

import (
	"context"
	"fmt"
	"time"
)

// ProviderCredentialRecord represents a stored provider credential.
type ProviderCredentialRecord struct {
	ID                string     `json:"id"`
	ProviderAccountID string     `json:"provider_account_id"`
	Version           int        `json:"version"`
	Status            string     `json:"status"` // active, pending_validation, revoked, rotated
	SecretRef         string     `json:"secret_ref,omitempty"`
	EncryptedBlob     []byte     `json:"-"` // never returned via API
	KMSKeyID          string     `json:"kms_key_id,omitempty"`
	Fingerprint       string     `json:"fingerprint"`
	ValidFrom         *time.Time `json:"valid_from,omitempty"`
	ValidUntil        *time.Time `json:"valid_until,omitempty"`
	LastUsedAt        *time.Time `json:"last_used_at,omitempty"`
	CreatedBy         string     `json:"created_by"`
	CreatedAt         time.Time  `json:"created_at"`
}

// ProviderCredentialStore provides CRUD access to provider credential storage.
type ProviderCredentialStore struct {
	pool *Pool
}

// NewProviderCredentialStore creates a new ProviderCredentialStore.
func NewProviderCredentialStore(pool *Pool) *ProviderCredentialStore {
	return &ProviderCredentialStore{pool: pool}
}

// List returns all credentials for a provider account.
func (s *ProviderCredentialStore) List(ctx context.Context, providerAccountID string) ([]*ProviderCredentialRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, provider_account_id, version, status, COALESCE(secret_ref, ''),
		       COALESCE(kms_key_id, ''), fingerprint, valid_from, valid_until, last_used_at,
		       created_by, created_at
		FROM provider_credentials
		WHERE provider_account_id = $1
		ORDER BY version DESC
	`, providerAccountID)
	if err != nil {
		return nil, fmt.Errorf("list credentials: %w", err)
	}
	defer rows.Close()

	var result []*ProviderCredentialRecord
	for rows.Next() {
		r := &ProviderCredentialRecord{}
		if err := rows.Scan(&r.ID, &r.ProviderAccountID, &r.Version, &r.Status, &r.SecretRef,
			&r.KMSKeyID, &r.Fingerprint, &r.ValidFrom, &r.ValidUntil, &r.LastUsedAt,
			&r.CreatedBy, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan credential: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// GetActive returns the currently active credential for a provider account.
func (s *ProviderCredentialStore) GetActive(ctx context.Context, providerAccountID string) (*ProviderCredentialRecord, error) {
	r := &ProviderCredentialRecord{}
	err := s.pool.QueryRow(ctx, `
		SELECT id, provider_account_id, version, status, COALESCE(secret_ref, ''),
		       encrypted_blob, COALESCE(kms_key_id, ''), fingerprint, valid_from, valid_until, last_used_at,
		       created_by, created_at
		FROM provider_credentials
		WHERE provider_account_id = $1 AND status = 'active'
		ORDER BY version DESC LIMIT 1
	`, providerAccountID).Scan(&r.ID, &r.ProviderAccountID, &r.Version, &r.Status, &r.SecretRef,
		&r.EncryptedBlob, &r.KMSKeyID, &r.Fingerprint, &r.ValidFrom, &r.ValidUntil, &r.LastUsedAt,
		&r.CreatedBy, &r.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("get active credential: %w", err)
	}
	return r, nil
}

// GetByID returns a provider credential by ID, including encrypted blob material.
func (s *ProviderCredentialStore) GetByID(ctx context.Context, credentialID string) (*ProviderCredentialRecord, error) {
	r := &ProviderCredentialRecord{}
	err := s.pool.QueryRow(ctx, `
		SELECT id, provider_account_id, version, status, COALESCE(secret_ref, ''),
		       encrypted_blob, COALESCE(kms_key_id, ''), fingerprint, valid_from, valid_until, last_used_at,
		       created_by, created_at
		FROM provider_credentials
		WHERE id = $1
	`, credentialID).Scan(&r.ID, &r.ProviderAccountID, &r.Version, &r.Status, &r.SecretRef,
		&r.EncryptedBlob, &r.KMSKeyID, &r.Fingerprint, &r.ValidFrom, &r.ValidUntil, &r.LastUsedAt,
		&r.CreatedBy, &r.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("get credential %s: %w", credentialID, err)
	}
	return r, nil
}

// NextVersion returns the next version number for a provider account.
func (s *ProviderCredentialStore) NextVersion(ctx context.Context, providerAccountID string) (int, error) {
	var version int
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(MAX(version), 0) + 1
		FROM provider_credentials
		WHERE provider_account_id = $1
	`, providerAccountID).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("get next credential version: %w", err)
	}
	return version, nil
}

// Insert creates a new provider credential.
func (s *ProviderCredentialStore) Insert(ctx context.Context, record *ProviderCredentialRecord) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO provider_credentials
			(id, provider_account_id, version, status, secret_ref, encrypted_blob,
			 kms_key_id, fingerprint, valid_from, valid_until, created_by, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`, record.ID, record.ProviderAccountID, record.Version, record.Status, nullableString(record.SecretRef), record.EncryptedBlob,
		nullableString(record.KMSKeyID), record.Fingerprint, record.ValidFrom, record.ValidUntil, record.CreatedBy, record.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert credential: %w", err)
	}
	return nil
}

// InsertActive creates a credential and rotates any currently active credential in one transaction.
func (s *ProviderCredentialStore) InsertActive(ctx context.Context, record *ProviderCredentialRecord) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin insert active credential: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err := tx.Exec(ctx, `
		UPDATE provider_credentials
		SET status = 'rotated'
		WHERE provider_account_id = $1 AND status = 'active'
	`, record.ProviderAccountID); err != nil {
		return fmt.Errorf("rotate previous credential: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO provider_credentials
			(id, provider_account_id, version, status, secret_ref, encrypted_blob,
			 kms_key_id, fingerprint, valid_from, valid_until, created_by, created_at)
		VALUES ($1, $2, $3, 'active', $4, $5, $6, $7, $8, $9, $10, $11)
	`, record.ID, record.ProviderAccountID, record.Version, nullableString(record.SecretRef), record.EncryptedBlob,
		nullableString(record.KMSKeyID), record.Fingerprint, record.ValidFrom, record.ValidUntil, record.CreatedBy, record.CreatedAt); err != nil {
		return fmt.Errorf("insert active credential: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit insert active credential: %w", err)
	}
	return nil
}

// Revoke sets a credential's status to 'revoked'.
func (s *ProviderCredentialStore) Revoke(ctx context.Context, credentialID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE provider_credentials SET status = 'revoked' WHERE id = $1`, credentialID)
	if err != nil {
		return fmt.Errorf("revoke credential: %w", err)
	}
	return nil
}

// MarkActive sets a credential's status to 'active'.
func (s *ProviderCredentialStore) MarkActive(ctx context.Context, credentialID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE provider_credentials SET status = 'active' WHERE id = $1`, credentialID)
	if err != nil {
		return fmt.Errorf("mark credential active: %w", err)
	}
	return nil
}

// UpdateEncryptedBlob updates the encrypted blob and fingerprint of an existing
// active credential row in place, keeping the row ID (and thus any
// provider-credential:<id> ref) stable. It is used to persist rotated OAuth2
// refresh tokens without invalidating the provider's CredentialID reference.
func (s *ProviderCredentialStore) UpdateEncryptedBlob(ctx context.Context, credentialID string, blob []byte, fingerprint string) error {
	if len(blob) == 0 {
		return fmt.Errorf("update credential %s: empty blob", credentialID)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE provider_credentials
		SET encrypted_blob = $1, fingerprint = $2
		WHERE id = $3 AND status = 'active'
	`, blob, fingerprint, credentialID)
	if err != nil {
		return fmt.Errorf("update credential %s: %w", credentialID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("update credential %s: no active row found", credentialID)
	}
	return nil
}
