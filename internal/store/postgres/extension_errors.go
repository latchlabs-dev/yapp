package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/earlgrey/piproxy/internal/id"
)

// ExtensionErrorRecord is an aggregated client-extension failure. Repeated
// reports for the same key/session/operation increment OccurrenceCount.
type ExtensionErrorRecord struct {
	ID               string    `json:"id"`
	TenantID         string    `json:"tenant_id"`
	TenantName       string    `json:"tenant_name,omitempty"`
	UserID           string    `json:"user_id,omitempty"`
	UserName         string    `json:"user_name,omitempty"`
	KeyID            string    `json:"key_id"`
	Operation        string    `json:"operation"`
	ErrorCode        string    `json:"error_code"`
	HTTPStatus       int       `json:"http_status"`
	Message          string    `json:"message,omitempty"`
	ProjectID        string    `json:"project_id,omitempty"`
	ConversationID   string    `json:"conversation_id,omitempty"`
	ExtensionVersion string    `json:"extension_version,omitempty"`
	OccurrenceCount  int64     `json:"occurrence_count"`
	FirstSeenAt      time.Time `json:"first_seen_at"`
	LastSeenAt       time.Time `json:"last_seen_at"`
}

// ExtensionErrorStore persists and lists aggregated extension failures.
type ExtensionErrorStore struct {
	pool *Pool
}

func NewExtensionErrorStore(pool *Pool) *ExtensionErrorStore {
	return &ExtensionErrorStore{pool: pool}
}

func (s *ExtensionErrorStore) Upsert(ctx context.Context, record *ExtensionErrorRecord) error {
	if record.ID == "" {
		record.ID = id.New("xerr")
	}
	now := time.Now()
	_, err := s.pool.Exec(ctx, `
		INSERT INTO extension_errors
			(id, tenant_id, user_id, key_id, operation, error_code,
			 http_status, message, project_id, conversation_id,
			 extension_version, occurrence_count, first_seen_at,
			 last_seen_at, created_at, updated_at)
		VALUES
			($1, $2, $3, $4, $5, $6,
			 $7, $8, $9, $10,
			 $11, 1, $12, $12, $12, $12)
		ON CONFLICT (tenant_id, key_id, operation, error_code, project_id, conversation_id)
		DO UPDATE SET
			user_id = COALESCE(EXCLUDED.user_id, extension_errors.user_id),
			http_status = EXCLUDED.http_status,
			message = EXCLUDED.message,
			extension_version = EXCLUDED.extension_version,
			occurrence_count = extension_errors.occurrence_count + 1,
			last_seen_at = EXCLUDED.last_seen_at,
			updated_at = EXCLUDED.updated_at
	`, record.ID, record.TenantID, nullableText(record.UserID), record.KeyID,
		record.Operation, record.ErrorCode, record.HTTPStatus, record.Message,
		record.ProjectID, record.ConversationID, record.ExtensionVersion, now)
	if err != nil {
		return fmt.Errorf("upsert extension error: %w", err)
	}
	return nil
}

func (s *ExtensionErrorStore) List(ctx context.Context, tenantID, operation string, limit, offset int) (int, []*ExtensionErrorRecord, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	where := "WHERE 1=1"
	args := make([]interface{}, 0, 4)
	if tenantID != "" {
		args = append(args, tenantID)
		where += fmt.Sprintf(" AND ee.tenant_id = $%d", len(args))
	}
	if operation != "" {
		args = append(args, operation)
		where += fmt.Sprintf(" AND ee.operation = $%d", len(args))
	}

	var total int
	if err := s.pool.QueryRow(ctx, "SELECT COUNT(*) FROM extension_errors ee "+where, args...).Scan(&total); err != nil {
		return 0, nil, fmt.Errorf("count extension errors: %w", err)
	}
	args = append(args, limit, offset)
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		SELECT ee.id, ee.tenant_id, COALESCE(t.name, ''),
		       COALESCE(ee.user_id, ''), COALESCE(u.display_name, ''), ee.key_id,
		       ee.operation, ee.error_code, ee.http_status, ee.message,
		       ee.project_id, ee.conversation_id, ee.extension_version,
		       ee.occurrence_count, ee.first_seen_at, ee.last_seen_at
		  FROM extension_errors ee
		  JOIN tenants t ON t.id = ee.tenant_id
		  LEFT JOIN users u ON u.id = ee.user_id
		  %s
		 ORDER BY ee.last_seen_at DESC
		 LIMIT $%d OFFSET $%d
	`, where, len(args)-1, len(args)), args...)
	if err != nil {
		return 0, nil, fmt.Errorf("list extension errors: %w", err)
	}
	defer rows.Close()

	records := make([]*ExtensionErrorRecord, 0)
	for rows.Next() {
		record := &ExtensionErrorRecord{}
		if err := rows.Scan(
			&record.ID, &record.TenantID, &record.TenantName,
			&record.UserID, &record.UserName, &record.KeyID,
			&record.Operation, &record.ErrorCode, &record.HTTPStatus, &record.Message,
			&record.ProjectID, &record.ConversationID, &record.ExtensionVersion,
			&record.OccurrenceCount, &record.FirstSeenAt, &record.LastSeenAt,
		); err != nil {
			return 0, nil, fmt.Errorf("scan extension error: %w", err)
		}
		records = append(records, record)
	}
	return total, records, rows.Err()
}
