package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/earlgrey/piproxy/internal/capture"
	"github.com/earlgrey/piproxy/internal/id"
)

// SessionSubmissionRecord is a Pi session submitted by the corp-proxy extension.
type SessionSubmissionRecord struct {
	ID               string     `json:"id"`
	TenantID         string     `json:"tenant_id,omitempty"`
	UserID           string     `json:"user_id,omitempty"`
	UserDisplayName  string     `json:"user_display_name,omitempty"`
	UserEmail        string     `json:"user_email,omitempty"`
	KeyID            string     `json:"key_id,omitempty"`
	OwnerID          string     `json:"-"`
	ConversationID   string     `json:"conversation_id"`
	SessionFile      string     `json:"session_file,omitempty"`
	MessageCount     int        `json:"message_count"`
	Model            string     `json:"model,omitempty"`
	TotalTokens      int64      `json:"total_tokens,omitempty"`
	TotalCostCents   float64    `json:"total_cost_cents,omitempty"`
	StartedAt        *time.Time `json:"started_at,omitempty"`
	EndedAt          *time.Time `json:"ended_at,omitempty"`
	ExtensionVersion string     `json:"extension_version,omitempty"`
	PiVersion        string     `json:"pi_version,omitempty"`
	ThinkingLevel    string     `json:"thinking_level,omitempty"` // Pi thinking level: off|minimal|low|medium|high|xhigh|max
	SessionName      string     `json:"session_name,omitempty"`   // human title set via /name
	FirstPrompt      string     `json:"first_prompt,omitempty"`   // truncated first user prompt (title fallback)
	RunMode          string     `json:"run_mode,omitempty"`       // tui|rpc|json|print
	Project          string     `json:"project,omitempty"`
	ProjectID        string     `json:"project_id,omitempty"`
	DistillAttempts  int        `json:"distill_attempts"`
	DistilledAt      *time.Time `json:"distilled_at,omitempty"`
	DistillError     string     `json:"distill_error,omitempty"`
	LastActivityAt   *time.Time `json:"last_activity_at,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
}

// SessionIdentity is the durable isolation boundary for a caller-controlled
// conversation ID. Conversation IDs are never globally authoritative.
type SessionIdentity struct {
	TenantID       string `json:"tenant_id"`
	OwnerID        string `json:"owner_id"`
	ConversationID string `json:"conversation_id"`
}

func (r *SessionSubmissionRecord) Identity() SessionIdentity {
	return SessionIdentity{TenantID: r.TenantID, OwnerID: r.OwnerID, ConversationID: r.ConversationID}
}

// SessionSubmissionStore persists Pi session data to Postgres.
type SessionSubmissionStore struct {
	pool *Pool
}

// NewSessionSubmissionStore creates a new session submission store.
func NewSessionSubmissionStore(pool *Pool) *SessionSubmissionStore {
	return &SessionSubmissionStore{pool: pool}
}

// Put stores or updates a session submission, keyed by tenant, owner, and
// conversation_id.
// Repeated submissions from the same Pi session (e.g., after each agent_end)
// update the same row so the latest stats always win.
func (s *SessionSubmissionStore) Put(ctx context.Context, record *SessionSubmissionRecord, summaryJSON []byte) error {
	if record.ID == "" {
		record.ID = id.New("ses")
	}

	summaryCompressed, err := capture.Compress(summaryJSON)
	if err != nil {
		return fmt.Errorf("compress session summary: %w", err)
	}

	_, err = s.pool.Exec(ctx, `
		INSERT INTO session_submissions
			(id, tenant_id, user_id, key_id, owner_id,
			 conversation_id, session_file, message_count, model,
			 total_tokens, total_cost_cents, started_at, ended_at,
			 summary_compressed, extension_version, pi_version,
			 thinking_level, session_name, first_prompt, run_mode,
			 project, project_id, created_at)
		VALUES
			($1, $2, $3, $4, $5,
			 $6, $7, $8, $9,
			 $10, $11, $12, $13,
			 $14, $15, $16,
			 $17, $18, $19, $20,
			 $21, $22, $23)
		ON CONFLICT (tenant_id, owner_id, conversation_id)
		WHERE tenant_id IS NOT NULL AND owner_id IS NOT NULL
		DO UPDATE SET
			user_id = EXCLUDED.user_id,
			key_id = EXCLUDED.key_id,
			session_file = COALESCE(EXCLUDED.session_file, session_submissions.session_file),
			message_count = EXCLUDED.message_count,
			model = COALESCE(EXCLUDED.model, session_submissions.model),
			total_tokens = EXCLUDED.total_tokens,
			total_cost_cents = EXCLUDED.total_cost_cents,
			started_at = COALESCE(EXCLUDED.started_at, session_submissions.started_at),
			ended_at = GREATEST(EXCLUDED.ended_at, session_submissions.ended_at),
			summary_compressed = COALESCE(EXCLUDED.summary_compressed, session_submissions.summary_compressed),
			extension_version = COALESCE(EXCLUDED.extension_version, session_submissions.extension_version),
			pi_version = COALESCE(EXCLUDED.pi_version, session_submissions.pi_version),
			thinking_level = COALESCE(EXCLUDED.thinking_level, session_submissions.thinking_level),
			session_name = COALESCE(EXCLUDED.session_name, session_submissions.session_name),
			first_prompt = COALESCE(EXCLUDED.first_prompt, session_submissions.first_prompt),
			run_mode = COALESCE(EXCLUDED.run_mode, session_submissions.run_mode),
			project = COALESCE(EXCLUDED.project, session_submissions.project),
			project_id = COALESCE(EXCLUDED.project_id, session_submissions.project_id)
	`, record.ID, record.TenantID, nullableText(record.UserID), nullableText(record.KeyID), record.OwnerID,
		record.ConversationID,
		nullableText(record.SessionFile),
		record.MessageCount,
		nullableText(record.Model),
		record.TotalTokens,
		record.TotalCostCents,
		record.StartedAt, record.EndedAt,
		summaryCompressed,
		nullableText(record.ExtensionVersion),
		nullableText(record.PiVersion),
		nullableText(record.ThinkingLevel),
		nullableText(record.SessionName),
		nullableText(record.FirstPrompt),
		nullableText(record.RunMode),
		nullableText(record.Project),
		nullableText(record.ProjectID),
		time.Now(),
	)
	if err != nil {
		return fmt.Errorf("upsert session submission: %w", err)
	}
	return nil
}

// List returns recent session submissions for a tenant, ordered by created_at DESC.
func (s *SessionSubmissionStore) List(ctx context.Context, tenantID string, limit, offset int) (total int, records []*SessionSubmissionRecord, err error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	where := "WHERE 1=1"
	args := []interface{}{}
	argIdx := 1

	if tenantID != "" {
		where = fmt.Sprintf(`WHERE ss.tenant_id = $%d`, argIdx)
		args = append(args, tenantID)
		argIdx++
	}

	// Count.
	countQuery := fmt.Sprintf(`SELECT COUNT(*) FROM session_submissions ss %s`, where)
	if err := s.pool.QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		return 0, nil, fmt.Errorf("count session submissions: %w", err)
	}

	query := fmt.Sprintf(`
		SELECT ss.id, COALESCE(ss.tenant_id, ''), COALESCE(ss.user_id, ''),
		       COALESCE(ss.key_id, ''), COALESCE(ss.owner_id, ''),
		       COALESCE(u.display_name, ''), COALESCE(u.email, ''),
		       ss.conversation_id, COALESCE(ss.session_file, ''),
		       ss.message_count, COALESCE(ss.model, ''),
		       COALESCE(ss.total_tokens, 0), COALESCE(ss.total_cost_cents, 0),
		       ss.started_at, ss.ended_at,
		       COALESCE(ss.extension_version, ''), COALESCE(ss.pi_version, ''),
		       COALESCE(ss.thinking_level, ''), COALESCE(ss.session_name, ''),
		       COALESCE(ss.first_prompt, ''), COALESCE(ss.run_mode, ''),
		       COALESCE(ss.project, ''), COALESCE(ss.project_id, ''), ss.distill_attempts,
		       ss.distilled_at, COALESCE(ss.distill_error, ''),
		       GREATEST(COALESCE(pa.last_payload_at, '-infinity'::timestamptz),
		                COALESCE(ss.ended_at, '-infinity'::timestamptz),
		                ss.created_at) AS last_activity_at,
		       ss.created_at
		  FROM session_submissions ss
		  LEFT JOIN users u ON u.id = ss.user_id
		  LEFT JOIN LATERAL (
			SELECT MAX(cp.created_at) AS last_payload_at
			  FROM conversation_payloads cp
			 WHERE cp.conversation_id = ss.conversation_id
			   AND cp.tenant_id = ss.tenant_id
			   AND ((ss.user_id IS NOT NULL AND cp.user_id = ss.user_id)
			     OR (ss.user_id IS NULL AND ss.key_id IS NOT NULL AND cp.key_id = ss.key_id))
		  ) pa ON true
		%s
		ORDER BY last_activity_at DESC
		LIMIT $%d OFFSET $%d
	`, where, argIdx, argIdx+1)
	args = append(args, limit, offset)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return 0, nil, fmt.Errorf("list session submissions: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		r := &SessionSubmissionRecord{}
		if err := rows.Scan(
			&r.ID, &r.TenantID, &r.UserID, &r.KeyID, &r.OwnerID,
			&r.UserDisplayName, &r.UserEmail,
			&r.ConversationID, &r.SessionFile,
			&r.MessageCount, &r.Model,
			&r.TotalTokens, &r.TotalCostCents,
			&r.StartedAt, &r.EndedAt,
			&r.ExtensionVersion, &r.PiVersion,
			&r.ThinkingLevel, &r.SessionName, &r.FirstPrompt, &r.RunMode,
			&r.Project, &r.ProjectID, &r.DistillAttempts,
			&r.DistilledAt, &r.DistillError,
			&r.LastActivityAt, &r.CreatedAt,
		); err != nil {
			return 0, nil, fmt.Errorf("scan session submission: %w", err)
		}
		records = append(records, r)
	}
	if records == nil {
		records = []*SessionSubmissionRecord{}
	}
	return total, records, rows.Err()
}

// TenantDistillationEnabled returns the explicit tenant consent flag. Global
// process configuration is checked separately before the queue is created.
func (s *SessionSubmissionStore) TenantDistillationEnabled(ctx context.Context, tenantID string) (bool, error) {
	var enabled bool
	if err := s.pool.QueryRow(ctx, `
		SELECT session_distillation_enabled
		  FROM tenants
		 WHERE id = $1 AND status <> 'deleted'
	`, tenantID).Scan(&enabled); err != nil {
		return false, fmt.Errorf("get tenant distillation consent: %w", err)
	}
	return enabled, nil
}

// GetByIdentity returns one owner-scoped session submission.
func (s *SessionSubmissionStore) GetByIdentity(ctx context.Context, identity SessionIdentity) (*SessionSubmissionRecord, []byte, error) {
	r := &SessionSubmissionRecord{}
	var summaryCompressed []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id, tenant_id, COALESCE(user_id, ''), COALESCE(key_id, ''), owner_id,
		       conversation_id, COALESCE(session_file, ''), message_count,
		       COALESCE(model, ''), COALESCE(total_tokens, 0),
		       COALESCE(total_cost_cents, 0), started_at, ended_at,
		       COALESCE(extension_version, ''), COALESCE(pi_version, ''),
		       COALESCE(thinking_level, ''), COALESCE(session_name, ''),
		       COALESCE(first_prompt, ''), COALESCE(run_mode, ''),
		       COALESCE(project, ''), COALESCE(project_id, ''), distill_attempts, distilled_at,
		       COALESCE(distill_error, ''), created_at, summary_compressed
		  FROM session_submissions
		 WHERE tenant_id = $1 AND owner_id = $2 AND conversation_id = $3
		 LIMIT 1
	`, identity.TenantID, identity.OwnerID, identity.ConversationID).Scan(
		&r.ID, &r.TenantID, &r.UserID, &r.KeyID, &r.OwnerID,
		&r.ConversationID, &r.SessionFile, &r.MessageCount, &r.Model,
		&r.TotalTokens, &r.TotalCostCents, &r.StartedAt, &r.EndedAt,
		&r.ExtensionVersion, &r.PiVersion,
		&r.ThinkingLevel, &r.SessionName, &r.FirstPrompt, &r.RunMode,
		&r.Project, &r.ProjectID, &r.DistillAttempts,
		&r.DistilledAt, &r.DistillError, &r.CreatedAt, &summaryCompressed,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("get owner-scoped session submission: %w", err)
	}
	summaryJSON, err := capture.Decompress(summaryCompressed)
	if err != nil {
		return nil, nil, fmt.Errorf("decompress session summary: %w", err)
	}
	return r, summaryJSON, nil
}

// ListUndistilledSessions returns only sessions whose tenant has explicitly
// enabled distillation. The stable ID cursor is internal to queue reconciliation.
func (s *SessionSubmissionStore) ListUndistilledSessions(ctx context.Context, afterID string, limit int) ([]*SessionSubmissionRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT ss.id, ss.tenant_id, COALESCE(ss.user_id, ''), COALESCE(ss.key_id, ''),
		       ss.owner_id, ss.conversation_id, COALESCE(ss.project, ''), COALESCE(ss.project_id, '')
		  FROM session_submissions ss
		  JOIN tenants t ON t.id = ss.tenant_id
		 WHERE ss.distilled_at IS NULL
		   AND ss.owner_id IS NOT NULL
		   AND t.status <> 'deleted'
		   AND t.session_distillation_enabled = true
		   AND ss.id > $1
		 ORDER BY ss.id ASC
		 LIMIT $2
	`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list consented undistilled sessions: %w", err)
	}
	defer rows.Close()
	var records []*SessionSubmissionRecord
	for rows.Next() {
		r := &SessionSubmissionRecord{}
		if err := rows.Scan(&r.ID, &r.TenantID, &r.UserID, &r.KeyID, &r.OwnerID, &r.ConversationID, &r.Project, &r.ProjectID); err != nil {
			return nil, fmt.Errorf("scan consented undistilled session: %w", err)
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

func (s *SessionSubmissionStore) RecordDistillAttemptFor(ctx context.Context, identity SessionIdentity) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE session_submissions
		   SET distill_attempts = distill_attempts + 1, distill_error = NULL
		 WHERE tenant_id = $1 AND owner_id = $2 AND conversation_id = $3
	`, identity.TenantID, identity.OwnerID, identity.ConversationID)
	if err != nil {
		return fmt.Errorf("record owner-scoped distillation attempt: %w", err)
	}
	return nil
}

func (s *SessionSubmissionStore) RecordDistillErrorFor(ctx context.Context, identity SessionIdentity, distillErr string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE session_submissions SET distill_error = $4
		 WHERE tenant_id = $1 AND owner_id = $2 AND conversation_id = $3
	`, identity.TenantID, identity.OwnerID, identity.ConversationID, nullableText(distillErr))
	if err != nil {
		return fmt.Errorf("record owner-scoped distillation error: %w", err)
	}
	return nil
}

func (s *SessionSubmissionStore) MarkDistilledFor(ctx context.Context, identity SessionIdentity, distillErr string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE session_submissions SET distilled_at = now(), distill_error = $4
		 WHERE tenant_id = $1 AND owner_id = $2 AND conversation_id = $3
	`, identity.TenantID, identity.OwnerID, identity.ConversationID, nullableText(distillErr))
	if err != nil {
		return fmt.Errorf("mark owner-scoped session distilled: %w", err)
	}
	return nil
}
