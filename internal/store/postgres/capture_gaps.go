package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/earlgrey/piproxy/internal/capture"
)

// CaptureGapStore persists capture gap markers: durable records that a
// captured payload was dropped or truncated at a point in time. Gaps carry no
// bodies, are tiny, and are intentionally never expired — they are the audit
// trail that explains missing conversation data (see migration 029).
type CaptureGapStore struct {
	pool *Pool
}

// NewCaptureGapStore creates a gap store on the given pool. All methods are
// nil-pool safe.
func NewCaptureGapStore(pool *Pool) *CaptureGapStore {
	return &CaptureGapStore{pool: pool}
}

// PutGaps batch-inserts gap markers. Empty batches are no-ops.
func (s *CaptureGapStore) PutGaps(ctx context.Context, gaps []*capture.Gap) error {
	if len(gaps) == 0 {
		return nil
	}
	if s.pool == nil {
		return errors.New("capture gap store: no database connection")
	}

	const columns = 11
	values := make([]string, 0, len(gaps))
	args := make([]interface{}, 0, len(gaps)*columns)
	for _, gap := range gaps {
		// Markers without an ID cannot be addressed later; the recorder always
		// assigns one, so anything empty here is malformed input, not data loss.
		if gap == nil || gap.ID == "" {
			continue
		}
		base := len(args)
		values = append(values, fmt.Sprintf(
			"($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
			base+1, base+2, base+3, base+4, base+5, base+6,
			base+7, base+8, base+9, base+10, base+11,
		))
		createdAt := gap.CreatedAt
		if createdAt.IsZero() {
			createdAt = time.Now().UTC()
		}
		args = append(args,
			gap.ID,
			nullableText(gap.ConversationID),
			nullableText(gap.TenantID),
			nullableText(gap.UserID),
			nullableText(gap.RequestID),
			gap.Provider,
			gap.Model,
			gap.Streaming,
			gap.Reason,
			gap.Detail,
			createdAt,
		)
	}
	if len(values) == 0 {
		return nil
	}

	query := `INSERT INTO capture_gaps
		(id, conversation_id, tenant_id, user_id, request_id,
		 provider, model, streaming, reason, detail, created_at)
		VALUES ` + strings.Join(values, ",")
	_, err := s.pool.Exec(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("insert capture gaps: %w", err)
	}
	return nil
}

// captureGapFilters carries normalized list filters.
type captureGapFilters struct {
	ConversationID string
	TenantID       string
	Reason         string
}

// List returns gap markers newest-first plus the total matching count.
func (s *CaptureGapStore) List(ctx context.Context, conversationID, tenantID, reason string, limit, offset int) ([]*capture.Gap, int, error) {
	if s.pool == nil {
		return nil, 0, errors.New("capture gap store: no database connection")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	filters := captureGapFilters{
		ConversationID: strings.TrimSpace(conversationID),
		TenantID:       strings.TrimSpace(tenantID),
		Reason:         strings.TrimSpace(reason),
	}
	where := []string{"1=1"}
	args := []interface{}{}
	if filters.ConversationID != "" {
		args = append(args, filters.ConversationID)
		where = append(where, fmt.Sprintf("conversation_id = $%d", len(args)))
	}
	if filters.TenantID != "" {
		args = append(args, filters.TenantID)
		where = append(where, fmt.Sprintf("tenant_id = $%d", len(args)))
	}
	if filters.Reason != "" {
		args = append(args, filters.Reason)
		where = append(where, fmt.Sprintf("reason = $%d", len(args)))
	}
	whereSQL := strings.Join(where, " AND ")

	var total int
	if err := s.pool.QueryRow(ctx,
		"SELECT count(*) FROM capture_gaps WHERE "+whereSQL, args...,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count capture gaps: %w", err)
	}

	listArgs := append([]interface{}{}, args...)
	listArgs = append(listArgs, limit, offset)
	rows, err := s.pool.Query(ctx, `
		SELECT id, COALESCE(conversation_id, ''), COALESCE(tenant_id, ''),
		       COALESCE(user_id, ''), COALESCE(request_id, ''),
		       provider, model, streaming, reason, detail, created_at
		FROM capture_gaps
		WHERE `+whereSQL+`
		ORDER BY created_at DESC
		LIMIT $`+fmt.Sprint(len(listArgs)-1)+` OFFSET $`+fmt.Sprint(len(listArgs)), listArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("list capture gaps: %w", err)
	}
	defer rows.Close()

	gaps := make([]*capture.Gap, 0, limit)
	for rows.Next() {
		gap := &capture.Gap{}
		if err := rows.Scan(
			&gap.ID, &gap.ConversationID, &gap.TenantID, &gap.UserID, &gap.RequestID,
			&gap.Provider, &gap.Model, &gap.Streaming, &gap.Reason, &gap.Detail, &gap.CreatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("scan capture gap: %w", err)
		}
		gaps = append(gaps, gap)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate capture gaps: %w", err)
	}
	return gaps, total, nil
}
