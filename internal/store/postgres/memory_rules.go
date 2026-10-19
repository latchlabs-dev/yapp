package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/earlgrey/piproxy/internal/id"
)

// MemoryInjectionRule controls whether context injection is enabled for a
// given project/tenant/user combination. NULL (empty string) columns match
// anything; the most specific matching rule wins.
type MemoryInjectionRule struct {
	ID           string    `json:"id"`
	Project      string    `json:"project,omitempty"`
	TenantID     string    `json:"tenant_id,omitempty"`
	UserID       string    `json:"user_id,omitempty"`
	Enabled      bool      `json:"enabled"`
	BudgetTokens *int      `json:"budget_tokens,omitempty"`
	Note         string    `json:"note,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// MemoryRuleStore persists context-injection rules.
type MemoryRuleStore struct {
	pool *Pool
}

// NewMemoryRuleStore creates a memory injection rule store.
func NewMemoryRuleStore(pool *Pool) *MemoryRuleStore {
	return &MemoryRuleStore{pool: pool}
}

// Upsert creates or updates a rule. Rules are unique per
// (project, tenant_id, user_id) scope; an existing rule for the same scope is
// updated in place so operators can toggle injection without accumulating rows.
func (s *MemoryRuleStore) Upsert(ctx context.Context, rule *MemoryInjectionRule) (*MemoryInjectionRule, error) {
	if rule.ID == "" {
		// Reuse an existing rule for the same scope if present.
		existing, err := s.findByScope(ctx, rule.Project, rule.TenantID, rule.UserID)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			rule.ID = existing.ID
		} else {
			rule.ID = id.New("mir")
		}
	}

	err := s.pool.QueryRow(ctx, `
		INSERT INTO memory_injection_rules
			(id, project, tenant_id, user_id, enabled, budget_tokens, note, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now(), now())
		ON CONFLICT (id) DO UPDATE SET
			project = EXCLUDED.project,
			tenant_id = EXCLUDED.tenant_id,
			user_id = EXCLUDED.user_id,
			enabled = EXCLUDED.enabled,
			budget_tokens = EXCLUDED.budget_tokens,
			note = EXCLUDED.note,
			updated_at = now()
		RETURNING created_at, updated_at
	`, rule.ID,
		nullableText(rule.Project), nullableText(rule.TenantID), nullableText(rule.UserID),
		rule.Enabled, rule.BudgetTokens, nullableText(rule.Note),
	).Scan(&rule.CreatedAt, &rule.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("upsert memory injection rule: %w", err)
	}
	return rule, nil
}

// findByScope returns the rule with exactly this scope, or nil.
func (s *MemoryRuleStore) findByScope(ctx context.Context, project, tenantID, userID string) (*MemoryInjectionRule, error) {
	rule := &MemoryInjectionRule{}
	err := s.pool.QueryRow(ctx, `
		SELECT id, COALESCE(project, ''), COALESCE(tenant_id, ''), COALESCE(user_id, ''),
		       enabled, budget_tokens, COALESCE(note, ''), created_at, updated_at
		  FROM memory_injection_rules
		 WHERE COALESCE(project, '') = $1
		   AND COALESCE(tenant_id, '') = $2
		   AND COALESCE(user_id, '') = $3
	`, project, tenantID, userID).Scan(
		&rule.ID, &rule.Project, &rule.TenantID, &rule.UserID,
		&rule.Enabled, &rule.BudgetTokens, &rule.Note, &rule.CreatedAt, &rule.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find memory injection rule by scope: %w", err)
	}
	return rule, nil
}

// List returns all rules, most recently updated first.
func (s *MemoryRuleStore) List(ctx context.Context) ([]*MemoryInjectionRule, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, COALESCE(project, ''), COALESCE(tenant_id, ''), COALESCE(user_id, ''),
		       enabled, budget_tokens, COALESCE(note, ''), created_at, updated_at
		  FROM memory_injection_rules
		 ORDER BY updated_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("list memory injection rules: %w", err)
	}
	defer rows.Close()

	var rules []*MemoryInjectionRule
	for rows.Next() {
		rule := &MemoryInjectionRule{}
		if err := rows.Scan(
			&rule.ID, &rule.Project, &rule.TenantID, &rule.UserID,
			&rule.Enabled, &rule.BudgetTokens, &rule.Note, &rule.CreatedAt, &rule.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan memory injection rule: %w", err)
		}
		rules = append(rules, rule)
	}
	if rules == nil {
		rules = []*MemoryInjectionRule{}
	}
	return rules, rows.Err()
}

// Match returns the most specific rule matching the given request scope, or
// nil when no rule matches. Specificity: each non-empty rule column that
// matches the request adds weight; project matches outweigh tenant, tenant
// outweighs user. Empty rule columns are wildcards.
func (s *MemoryRuleStore) Match(ctx context.Context, project, tenantID, userID string) (*MemoryInjectionRule, error) {
	rule := &MemoryInjectionRule{}
	err := s.pool.QueryRow(ctx, `
		SELECT id, COALESCE(project, ''), COALESCE(tenant_id, ''), COALESCE(user_id, ''),
		       enabled, budget_tokens, COALESCE(note, ''), created_at, updated_at
		  FROM memory_injection_rules
		 WHERE (project IS NULL OR project = $1)
		   AND (tenant_id IS NULL OR tenant_id = $2)
		   AND (user_id IS NULL OR user_id = $3)
		 ORDER BY (project IS NOT NULL)::int * 4
		        + (tenant_id IS NOT NULL)::int * 2
		        + (user_id IS NOT NULL)::int DESC,
		          updated_at DESC
		 LIMIT 1
	`, project, tenantID, userID).Scan(
		&rule.ID, &rule.Project, &rule.TenantID, &rule.UserID,
		&rule.Enabled, &rule.BudgetTokens, &rule.Note, &rule.CreatedAt, &rule.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("match memory injection rule: %w", err)
	}
	return rule, nil
}

// Delete removes a rule by ID.
func (s *MemoryRuleStore) Delete(ctx context.Context, ruleID string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM memory_injection_rules WHERE id = $1`, ruleID)
	if err != nil {
		return fmt.Errorf("delete memory injection rule: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("memory injection rule %s: %w", ruleID, pgx.ErrNoRows)
	}
	return nil
}
