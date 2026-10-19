package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/earlgrey/piproxy/internal/quota"
)

// QuotaPolicyRecord represents a stored quota policy.
type QuotaPolicyRecord struct {
	ID        string          `json:"id"`
	TenantID  string          `json:"tenant_id"`
	ScopeType string          `json:"scope_type"`
	ScopeID   string          `json:"scope_id"`
	Priority  int             `json:"priority"`
	Limits    json.RawMessage `json:"limits"` // {"requests_per_minute": 60, ...}
	Enabled   bool            `json:"enabled"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// QuotaPolicyStore provides CRUD access to quota policy storage.
type QuotaPolicyStore struct {
	pool *Pool
}

// NewQuotaPolicyStore creates a new QuotaPolicyStore.
func NewQuotaPolicyStore(pool *Pool) *QuotaPolicyStore {
	return &QuotaPolicyStore{pool: pool}
}

// List returns all quota policies for a tenant.
func (s *QuotaPolicyStore) List(ctx context.Context, tenantID string) ([]*QuotaPolicyRecord, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, tenant_id, scope_type, scope_id, priority, limits, enabled, created_at, updated_at
		 FROM quota_policies WHERE tenant_id = $1 ORDER BY priority`,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("list quota policies: %w", err)
	}
	defer rows.Close()

	var result []*QuotaPolicyRecord
	for rows.Next() {
		r := &QuotaPolicyRecord{}
		if err := rows.Scan(&r.ID, &r.TenantID, &r.ScopeType, &r.ScopeID, &r.Priority, &r.Limits, &r.Enabled, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan quota policy: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// ListAll returns all quota policies across tenants for data-plane enforcement.
func (s *QuotaPolicyStore) ListAll(ctx context.Context) ([]*QuotaPolicyRecord, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, tenant_id, scope_type, scope_id, priority, limits, enabled, created_at, updated_at
		 FROM quota_policies ORDER BY tenant_id, priority`,
	)
	if err != nil {
		return nil, fmt.Errorf("list all quota policies: %w", err)
	}
	defer rows.Close()

	var result []*QuotaPolicyRecord
	for rows.Next() {
		r := &QuotaPolicyRecord{}
		if err := rows.Scan(&r.ID, &r.TenantID, &r.ScopeType, &r.ScopeID, &r.Priority, &r.Limits, &r.Enabled, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan quota policy: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// GetByID retrieves a quota policy by ID.
func (s *QuotaPolicyStore) GetByID(ctx context.Context, tenantID, policyID string) (*QuotaPolicyRecord, error) {
	r := &QuotaPolicyRecord{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, tenant_id, scope_type, scope_id, priority, limits, enabled, created_at, updated_at
		 FROM quota_policies WHERE id = $1 AND tenant_id = $2`,
		policyID, tenantID,
	).Scan(&r.ID, &r.TenantID, &r.ScopeType, &r.ScopeID, &r.Priority, &r.Limits, &r.Enabled, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("get quota policy %s: %w", policyID, err)
	}
	return r, nil
}

// Insert creates a new quota policy.
func (s *QuotaPolicyStore) Insert(ctx context.Context, record *QuotaPolicyRecord) error {
	limitsJSON, err := json.Marshal(record.Limits)
	if err != nil {
		return fmt.Errorf("marshal limits: %w", err)
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO quota_policies (id, tenant_id, scope_type, scope_id, priority, limits, enabled, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		record.ID, record.TenantID, record.ScopeType, record.ScopeID, record.Priority, limitsJSON, record.Enabled, record.CreatedAt, record.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert quota policy: %w", err)
	}
	return nil
}

// Update modifies an existing quota policy.
func (s *QuotaPolicyStore) Update(ctx context.Context, record *QuotaPolicyRecord) error {
	record.UpdatedAt = time.Now()
	limitsJSON, err := json.Marshal(record.Limits)
	if err != nil {
		return fmt.Errorf("marshal limits: %w", err)
	}
	_, err = s.pool.Exec(ctx,
		`UPDATE quota_policies SET scope_type=$3, scope_id=$4, priority=$5, limits=$6, enabled=$7, updated_at=$8
		 WHERE id=$1 AND tenant_id=$2`,
		record.ID, record.TenantID, record.ScopeType, record.ScopeID, record.Priority, limitsJSON, record.Enabled, record.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("update quota policy: %w", err)
	}
	return nil
}

// Delete removes a quota policy by ID.
func (s *QuotaPolicyStore) Delete(ctx context.Context, tenantID, policyID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM quota_policies WHERE id = $1 AND tenant_id = $2`, policyID, tenantID)
	if err != nil {
		return fmt.Errorf("delete quota policy: %w", err)
	}
	return nil
}

// ToPolicy converts a stored record to a quota.Policy for the enforcer.
func (r *QuotaPolicyRecord) ToPolicy() (*quota.Policy, error) {
	var limits map[quota.LimitType]int64
	if err := json.Unmarshal(r.Limits, &limits); err != nil {
		return nil, fmt.Errorf("unmarshal limits: %w", err)
	}
	return &quota.Policy{
		ID:        r.ID,
		TenantID:  r.TenantID,
		ScopeType: quota.ScopeType(r.ScopeType),
		ScopeID:   r.ScopeID,
		Priority:  r.Priority,
		Limits:    limits,
		Enabled:   r.Enabled,
	}, nil
}
