package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/earlgrey/piproxy/internal/modelaccess"
)

// TenantModelSettingsStore persists tenant model allow/deny settings.
type TenantModelSettingsStore struct {
	pool *Pool
}

func NewTenantModelSettingsStore(pool *Pool) *TenantModelSettingsStore {
	return &TenantModelSettingsStore{pool: pool}
}

func (s *TenantModelSettingsStore) Get(ctx context.Context, tenantID string) (*modelaccess.Config, error) {
	cfg := &modelaccess.Config{}
	err := s.pool.QueryRow(ctx, `
		SELECT tenant_id, mode, model_ids, created_at, updated_at
		FROM tenant_model_settings
		WHERE tenant_id = $1
	`, tenantID).Scan(&cfg.TenantID, &cfg.Mode, &cfg.ModelIDs, &cfg.CreatedAt, &cfg.UpdatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return modelaccess.DefaultConfig(tenantID), nil
		}
		return nil, fmt.Errorf("get tenant model settings: %w", err)
	}
	cfg.Mode = modelaccess.NormalizeMode(cfg.Mode)
	if cfg.ModelIDs == nil {
		cfg.ModelIDs = []string{}
	}
	return cfg, nil
}

func (s *TenantModelSettingsStore) Upsert(ctx context.Context, cfg *modelaccess.Config) (*modelaccess.Config, error) {
	now := time.Now()
	mode := modelaccess.NormalizeMode(cfg.Mode)
	modelIDs := sanitizeModelIDs(cfg.ModelIDs)

	row := s.pool.QueryRow(ctx, `
		INSERT INTO tenant_model_settings (tenant_id, mode, model_ids, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $4)
		ON CONFLICT (tenant_id) DO UPDATE SET
			mode = EXCLUDED.mode,
			model_ids = EXCLUDED.model_ids,
			updated_at = EXCLUDED.updated_at
		RETURNING tenant_id, mode, model_ids, created_at, updated_at
	`, cfg.TenantID, mode, modelIDs, now)

	out := &modelaccess.Config{}
	if err := row.Scan(&out.TenantID, &out.Mode, &out.ModelIDs, &out.CreatedAt, &out.UpdatedAt); err != nil {
		return nil, fmt.Errorf("upsert tenant model settings: %w", err)
	}
	if out.ModelIDs == nil {
		out.ModelIDs = []string{}
	}
	return out, nil
}

func sanitizeModelIDs(ids []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
