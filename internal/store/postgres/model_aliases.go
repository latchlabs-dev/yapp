package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/earlgrey/piproxy/internal/providers"
)

// ModelAliasRecord represents a stored model alias.
type ModelAliasRecord struct {
	ID                string          `json:"id"`
	TenantID          string          `json:"tenant_id,omitempty"`
	PublicProvider    string          `json:"public_provider"`
	PublicModel       string          `json:"public_model"`
	UpstreamProvider  string          `json:"upstream_provider"`
	UpstreamModel     string          `json:"upstream_model"`
	ProviderAccountID string          `json:"provider_account_id"`
	APIType           string          `json:"api_type"`
	Capabilities      json.RawMessage `json:"capabilities,omitempty"`
	Cost              json.RawMessage `json:"cost,omitempty"`
	Limits            json.RawMessage `json:"limits,omitempty"`
	Status            string          `json:"status"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
}

// ModelAliasStore provides CRUD access to model alias storage.
type ModelAliasStore struct {
	pool *Pool
}

// NewModelAliasStore creates a new ModelAliasStore.
func NewModelAliasStore(pool *Pool) *ModelAliasStore {
	return &ModelAliasStore{pool: pool}
}

// List returns all model aliases for a tenant (or global).
func (s *ModelAliasStore) List(ctx context.Context, tenantID string) ([]*ModelAliasRecord, error) {
	return s.list(ctx,
		`SELECT id, tenant_id, public_provider, public_model,
		        upstream_provider, upstream_model, provider_account_id, api_type,
		        capabilities, cost, limits, status, created_at, updated_at
		 FROM model_aliases
		 WHERE tenant_id IS NULL OR tenant_id = $1
		 ORDER BY public_provider, public_model`,
		tenantID,
	)
}

// ListAll returns every global and tenant-scoped synthetic model for the admin
// routing UI.
func (s *ModelAliasStore) ListAll(ctx context.Context) ([]*ModelAliasRecord, error) {
	return s.list(ctx,
		`SELECT id, tenant_id, public_provider, public_model,
		        upstream_provider, upstream_model, provider_account_id, api_type,
		        capabilities, cost, limits, status, created_at, updated_at
		 FROM model_aliases
		 ORDER BY tenant_id NULLS FIRST, public_model`,
	)
}

func (s *ModelAliasStore) list(ctx context.Context, query string, args ...interface{}) ([]*ModelAliasRecord, error) {
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list model aliases: %w", err)
	}
	defer rows.Close()

	var result []*ModelAliasRecord
	for rows.Next() {
		r := &ModelAliasRecord{}
		var tenantIDPtr *string
		if err := rows.Scan(&r.ID, &tenantIDPtr, &r.PublicProvider, &r.PublicModel,
			&r.UpstreamProvider, &r.UpstreamModel, &r.ProviderAccountID, &r.APIType,
			&r.Capabilities, &r.Cost, &r.Limits, &r.Status, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan model alias: %w", err)
		}
		if tenantIDPtr != nil {
			r.TenantID = *tenantIDPtr
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// GetByID retrieves a model alias by ID.
func (s *ModelAliasStore) GetByID(ctx context.Context, id string) (*ModelAliasRecord, error) {
	r := &ModelAliasRecord{}
	var tenantIDPtr *string
	err := s.pool.QueryRow(ctx,
		`SELECT id, tenant_id, public_provider, public_model,
		        upstream_provider, upstream_model, provider_account_id, api_type,
		        capabilities, cost, limits, status, created_at, updated_at
		 FROM model_aliases WHERE id = $1`,
		id,
	).Scan(&r.ID, &tenantIDPtr, &r.PublicProvider, &r.PublicModel,
		&r.UpstreamProvider, &r.UpstreamModel, &r.ProviderAccountID, &r.APIType,
		&r.Capabilities, &r.Cost, &r.Limits, &r.Status, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("get model alias %s: %w", id, err)
	}
	if tenantIDPtr != nil {
		r.TenantID = *tenantIDPtr
	}
	return r, nil
}

// Insert creates a new model alias.
func (s *ModelAliasStore) Insert(ctx context.Context, record *ModelAliasRecord) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO model_aliases
		 (id, tenant_id, public_provider, public_model, upstream_provider, upstream_model,
		  provider_account_id, api_type, capabilities, cost, limits, status, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		record.ID, nullableString(record.TenantID), record.PublicProvider, record.PublicModel,
		record.UpstreamProvider, record.UpstreamModel, record.ProviderAccountID, record.APIType,
		record.Capabilities, record.Cost, record.Limits, record.Status, record.CreatedAt, record.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert model alias: %w", err)
	}
	return nil
}

// Update modifies an existing model alias.
func (s *ModelAliasStore) Update(ctx context.Context, record *ModelAliasRecord) error {
	record.UpdatedAt = time.Now()
	_, err := s.pool.Exec(ctx,
		`UPDATE model_aliases SET
		 public_provider=$3, public_model=$4, upstream_provider=$5, upstream_model=$6,
		 provider_account_id=$7, api_type=$8, capabilities=$9, cost=$10, limits=$11,
		 status=$12, updated_at=$13
		 WHERE id=$1 AND (tenant_id=$2 OR (tenant_id IS NULL AND $2 IS NULL))`,
		record.ID, nullableString(record.TenantID),
		record.PublicProvider, record.PublicModel,
		record.UpstreamProvider, record.UpstreamModel,
		record.ProviderAccountID, record.APIType,
		record.Capabilities, record.Cost, record.Limits,
		record.Status, record.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("update model alias: %w", err)
	}
	return nil
}

// Delete removes a model alias by ID.
func (s *ModelAliasStore) Delete(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM model_aliases WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete model alias: %w", err)
	}
	return nil
}

// ToAlias converts a stored record to a providers.ModelAlias.
func (r *ModelAliasRecord) ToAlias() *providers.ModelAlias {
	return &providers.ModelAlias{
		ID:                r.ID,
		TenantID:          r.TenantID,
		PublicProvider:    r.PublicProvider,
		PublicModel:       r.PublicModel,
		UpstreamProvider:  r.UpstreamProvider,
		UpstreamModel:     r.UpstreamModel,
		ProviderAccountID: r.ProviderAccountID,
		APIType:           r.APIType,
		Enabled:           r.Status == "active",
	}
}

// nullableString returns nil for empty strings (for nullable DB columns).
func nullableString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
