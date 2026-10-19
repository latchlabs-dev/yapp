package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/earlgrey/piproxy/internal/audit"
	"github.com/earlgrey/piproxy/internal/id"
)

// AuditStore persists audit events to Postgres.
type AuditStore struct {
	pool *Pool
}

// NewAuditStore creates a new AuditStore.
func NewAuditStore(pool *Pool) *AuditStore {
	return &AuditStore{pool: pool}
}

// InsertAuditEvent writes a single audit event to the database.
func (s *AuditStore) InsertAuditEvent(ctx context.Context, event *audit.Event) error {
	if event.ID == "" {
		event.ID = id.New("aev_")
	}

	redactedDiffJSON := []byte("null")
	if event.RedactedDiff != nil {
		diffBytes, err := json.Marshal(event.RedactedDiff)
		if err == nil {
			redactedDiffJSON = diffBytes
		}
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO audit_events
			(id, tenant_id, actor_type, actor_id, action,
			 target_type, target_id, result, redacted_diff, request_id, created_at)
		VALUES
			($1, $2, $3, $4, $5,
			 $6, $7, $8, $9, $10, $11)
	`,
		event.ID, nullableText(event.TenantID), event.ActorType, event.ActorID, event.Action,
		event.TargetType, nullableText(event.TargetID), event.Result, string(redactedDiffJSON), nullableText(event.RequestID), event.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert audit event: %w", err)
	}
	return nil
}
