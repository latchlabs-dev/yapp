package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/earlgrey/piproxy/internal/blob"
	"github.com/earlgrey/piproxy/internal/capture"
	"github.com/earlgrey/piproxy/internal/id"
	"github.com/earlgrey/piproxy/internal/sessionarchive"
)

const maxPromptSummaryRunes = 2000

// BodyLocationPostgres is conversation_payloads.body_location (and
// pi_session_logs.body_location) for bodies held in the bytea columns. Any
// other value is the object store location holding them.
const BodyLocationPostgres = "postgres"

const gzipContentType = "application/gzip"

// ConversationPayloadStore persists raw conversation payloads: metadata in
// Postgres, gzip bodies in the bytea columns and/or object storage.
type ConversationPayloadStore struct {
	pool          *Pool
	objects       blob.Objects
	rawInObjects  bool
	rawInPostgres bool
}

// NewConversationPayloadStore creates a capture payload store that writes
// bodies to Postgres.
func NewConversationPayloadStore(pool *Pool) *ConversationPayloadStore {
	return &ConversationPayloadStore{pool: pool, rawInPostgres: true}
}

// WithObjects attaches object storage. Reads follow each row's body_location
// regardless; inObjects/inPostgres choose where new bodies are written.
func (s *ConversationPayloadStore) WithObjects(objects blob.Objects, inObjects, inPostgres bool) *ConversationPayloadStore {
	s.objects = objects
	s.rawInObjects = inObjects && objects != nil
	s.rawInPostgres = inPostgres || !s.rawInObjects
	return s
}

var _ sessionarchive.PayloadSource = (*ConversationPayloadStore)(nil)

// storedBodies is where one capture's bodies are written.
type storedBodies struct {
	location    string
	request     []byte // gzip; nil when not kept in Postgres
	response    []byte
	requestKey  string
	responseKey string
}

// Put stores a payload. Bodies are gzip-compressed and written to the bytea
// columns, object storage, or both, as configured by WithObjects.
// Object-backed captures are archival and kept forever: their expires_at is
// cleared so the retention sweeper never deletes the row that locates them.
func (s *ConversationPayloadStore) Put(ctx context.Context, payload *capture.Payload) error {
	if payload.ID == "" {
		payload.ID = id.New("cap")
	}
	if payload.ResponseFormat == "" {
		payload.ResponseFormat = capture.ResponseFormatJSON
	}
	if payload.CreatedAt.IsZero() {
		payload.CreatedAt = time.Now()
	}

	reqCompressed, err := capture.Compress(payload.RequestBody)
	if err != nil {
		return fmt.Errorf("compress request payload: %w", err)
	}
	respCompressed, err := capture.Compress(payload.ResponseBody)
	if err != nil {
		return fmt.Errorf("compress response payload: %w", err)
	}

	bodies := storedBodies{location: BodyLocationPostgres, request: reqCompressed, response: respCompressed}
	if s.rawInObjects {
		bodies.requestKey = sessionarchive.CaptureRequestKey(payload.TenantID, payload.CreatedAt, payload.RequestID)
		bodies.responseKey = sessionarchive.CaptureResponseKey(payload.TenantID, payload.CreatedAt, payload.RequestID)
		if err := s.objects.Put(ctx, bodies.requestKey, reqCompressed, gzipContentType); err != nil {
			return fmt.Errorf("store request body object: %w", err)
		}
		if err := s.objects.Put(ctx, bodies.responseKey, respCompressed, gzipContentType); err != nil {
			return fmt.Errorf("store response body object: %w", err)
		}
		bodies.location = s.objects.Location()
		if !s.rawInPostgres {
			bodies.request, bodies.response = nil, nil
		}
		payload.ExpiresAt = nil
	}

	_, err = s.pool.Exec(ctx, `
		INSERT INTO conversation_payloads
			(id, request_id, conversation_id, tenant_id, user_id, key_id,
			 provider, model, status, error_class, streaming,
			 request_body_compressed, response_body_compressed,
			 request_truncated, response_truncated, response_format,
			 created_at, completed_at, expires_at,
			 body_location, request_object_key, response_object_key,
			 request_bytes, response_bytes)
		VALUES
			($1, $2, $3, $4, $5, $6,
			 $7, $8, $9, $10, $11,
			 $12, $13,
			 $14, $15, $16,
			 $17, $18, $19,
			 $20, $21, $22,
			 $23, $24)
		ON CONFLICT (request_id) DO UPDATE SET
				conversation_id = COALESCE(EXCLUDED.conversation_id, conversation_payloads.conversation_id),
				tenant_id = COALESCE(EXCLUDED.tenant_id, conversation_payloads.tenant_id),
				user_id = COALESCE(EXCLUDED.user_id, conversation_payloads.user_id),
				key_id = COALESCE(EXCLUDED.key_id, conversation_payloads.key_id),
				provider = EXCLUDED.provider,
				model = EXCLUDED.model,
				status = EXCLUDED.status,
				error_class = EXCLUDED.error_class,
				streaming = EXCLUDED.streaming,
				request_body_compressed = EXCLUDED.request_body_compressed,
				response_body_compressed = EXCLUDED.response_body_compressed,
				request_truncated = EXCLUDED.request_truncated,
				response_truncated = EXCLUDED.response_truncated,
				response_format = EXCLUDED.response_format,
				completed_at = EXCLUDED.completed_at,
				expires_at = EXCLUDED.expires_at,
				body_location = EXCLUDED.body_location,
				request_object_key = EXCLUDED.request_object_key,
				response_object_key = EXCLUDED.response_object_key,
				request_bytes = EXCLUDED.request_bytes,
				response_bytes = EXCLUDED.response_bytes
	`, conversationPayloadArgs(payload, bodies)...)
	if err != nil {
		return fmt.Errorf("insert conversation payload: %w", err)
	}
	return nil
}

// Get retrieves and decompresses a payload by request ID, reading bodies from
// wherever the row says they live.
func (s *ConversationPayloadStore) Get(ctx context.Context, requestID string) (*capture.Payload, error) {
	var payload capture.Payload
	var reqCompressed, respCompressed []byte
	var reqKey, respKey string

	err := s.pool.QueryRow(ctx, `
		SELECT id, request_id, COALESCE(conversation_id, ''),
		       COALESCE(tenant_id, ''), COALESCE(user_id, ''), COALESCE(key_id, ''),
		       provider, model, status, COALESCE(error_class, ''), streaming,
		       request_body_compressed, response_body_compressed,
		       request_truncated, response_truncated, response_format,
		       created_at, completed_at, expires_at,
		       COALESCE(request_object_key, ''), COALESCE(response_object_key, '')
		  FROM conversation_payloads
		 WHERE request_id = $1
	`, requestID).Scan(
		&payload.ID, &payload.RequestID, &payload.ConversationID,
		&payload.TenantID, &payload.UserID, &payload.KeyID,
		&payload.Provider, &payload.Model, &payload.Status, &payload.ErrorClass, &payload.Streaming,
		&reqCompressed, &respCompressed,
		&payload.RequestTruncated, &payload.ResponseTruncated, &payload.ResponseFormat,
		&payload.CreatedAt, &payload.CompletedAt, &payload.ExpiresAt,
		&reqKey, &respKey,
	)
	if err != nil {
		return nil, fmt.Errorf("get conversation payload %s: %w", requestID, err)
	}
	if err := s.hydrateBodies(ctx, &payload, reqCompressed, respCompressed, reqKey, respKey); err != nil {
		return nil, err
	}
	return &payload, nil
}

// hydrateBodies decompresses a capture's bodies from their bytea columns or,
// when those are NULL, from object storage.
func (s *ConversationPayloadStore) hydrateBodies(ctx context.Context, payload *capture.Payload, reqCompressed, respCompressed []byte, reqKey, respKey string) error {
	var err error
	if payload.RequestBody, err = s.body(ctx, reqCompressed, reqKey); err != nil {
		return fmt.Errorf("request payload: %w", err)
	}
	if payload.ResponseBody, err = s.body(ctx, respCompressed, respKey); err != nil {
		return fmt.Errorf("response payload: %w", err)
	}
	return nil
}

func (s *ConversationPayloadStore) body(ctx context.Context, compressed []byte, objectKey string) ([]byte, error) {
	if compressed == nil && objectKey != "" {
		if s.objects == nil {
			return nil, fmt.Errorf("body %s is in object storage, which is not configured", objectKey)
		}
		data, err := s.objects.Read(ctx, objectKey)
		if err != nil {
			return nil, fmt.Errorf("read body object %s: %w", objectKey, err)
		}
		compressed = data
	}
	if compressed == nil {
		return nil, blob.ErrNotFound
	}
	body, err := capture.Decompress(compressed)
	if err != nil {
		return nil, fmt.Errorf("decompress: %w", err)
	}
	return body, nil
}

// ListConversationRequests implements sessionarchive.PayloadSource.
func (s *ConversationPayloadStore) ListConversationRequests(ctx context.Context, tenantID, conversationID string) ([]sessionarchive.PayloadRef, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT request_id, created_at
		  FROM conversation_payloads
		 WHERE conversation_id = $2 AND COALESCE(tenant_id, '') = $1
		 ORDER BY created_at, request_id
	`, tenantID, conversationID)
	if err != nil {
		return nil, fmt.Errorf("list conversation requests: %w", err)
	}
	defer rows.Close()
	var refs []sessionarchive.PayloadRef
	for rows.Next() {
		var ref sessionarchive.PayloadRef
		if err := rows.Scan(&ref.RequestID, &ref.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan conversation request: %w", err)
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

// HasRequestsBefore implements sessionarchive.PayloadSource.
func (s *ConversationPayloadStore) HasRequestsBefore(ctx context.Context, tenantID, conversationID string, before time.Time, excludeRequestID string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM conversation_payloads
			 WHERE conversation_id = $2 AND COALESCE(tenant_id, '') = $1
			   AND created_at < $3 AND request_id <> $4
		)
	`, tenantID, conversationID, before, excludeRequestID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check earlier conversation requests: %w", err)
	}
	return exists, nil
}

// LoadPayload implements sessionarchive.PayloadSource. A capture whose row or
// body object has disappeared reports blob.ErrNotFound so rebuilds fail closed.
func (s *ConversationPayloadStore) LoadPayload(ctx context.Context, requestID string) (*capture.Payload, error) {
	payload, err := s.Get(ctx, requestID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %v", blob.ErrNotFound, err)
	}
	return payload, err
}

// ListByConversation returns decompressed payloads for a conversation,
// newest first. Used by the memory distiller: the most recent request body
// carries the full message history of the session.
func (s *ConversationPayloadStore) ListByConversation(ctx context.Context, conversationID string, limit int) ([]*capture.Payload, error) {
	if limit <= 0 {
		limit = 1
	}

	rows, err := s.pool.Query(ctx, `
		SELECT id, request_id, COALESCE(conversation_id, ''),
		       COALESCE(tenant_id, ''), COALESCE(user_id, ''), COALESCE(key_id, ''),
		       provider, model, status, COALESCE(error_class, ''), streaming,
		       request_body_compressed, response_body_compressed,
		       request_truncated, response_truncated, response_format,
		       created_at, completed_at, expires_at,
		       COALESCE(request_object_key, ''), COALESCE(response_object_key, '')
		  FROM conversation_payloads
		 WHERE conversation_id = $1
		 ORDER BY created_at DESC
		 LIMIT $2
	`, conversationID, limit)
	if err != nil {
		return nil, fmt.Errorf("list payloads for conversation %s: %w", conversationID, err)
	}
	defer rows.Close()

	var payloads []*capture.Payload
	for rows.Next() {
		var payload capture.Payload
		var reqCompressed, respCompressed []byte
		var reqKey, respKey string
		if err := rows.Scan(
			&payload.ID, &payload.RequestID, &payload.ConversationID,
			&payload.TenantID, &payload.UserID, &payload.KeyID,
			&payload.Provider, &payload.Model, &payload.Status, &payload.ErrorClass, &payload.Streaming,
			&reqCompressed, &respCompressed,
			&payload.RequestTruncated, &payload.ResponseTruncated, &payload.ResponseFormat,
			&payload.CreatedAt, &payload.CompletedAt, &payload.ExpiresAt,
			&reqKey, &respKey,
		); err != nil {
			return nil, fmt.Errorf("scan conversation payload: %w", err)
		}
		if err := s.hydrateBodies(ctx, &payload, reqCompressed, respCompressed, reqKey, respKey); err != nil {
			return nil, err
		}
		payloads = append(payloads, &payload)
	}
	return payloads, rows.Err()
}

// ListBySession returns payloads inside the authenticated owner boundary for a
// caller-controlled conversation ID. Distillation must use this method rather
// than the admin-oriented global lookup above.
func (s *ConversationPayloadStore) ListBySession(ctx context.Context, rec *SessionSubmissionRecord, limit int) ([]*capture.Payload, error) {
	if limit <= 0 {
		limit = 1
	}
	ownerWhere := "cp.key_id = $4"
	ownerValue := rec.KeyID
	if rec.UserID != "" {
		ownerWhere = "cp.user_id = $4"
		ownerValue = rec.UserID
	}
	query := fmt.Sprintf(`
		SELECT id, request_id, COALESCE(conversation_id, ''),
		       COALESCE(tenant_id, ''), COALESCE(user_id, ''), COALESCE(key_id, ''),
		       provider, model, status, COALESCE(error_class, ''), streaming,
		       request_body_compressed, response_body_compressed,
		       request_truncated, response_truncated, response_format,
		       created_at, completed_at, expires_at,
		       COALESCE(request_object_key, ''), COALESCE(response_object_key, '')
		  FROM conversation_payloads cp
		 WHERE cp.tenant_id = $1 AND cp.conversation_id = $2 AND %s
		 ORDER BY created_at DESC
		 LIMIT $3
	`, ownerWhere)
	rows, err := s.pool.Query(ctx, query, rec.TenantID, rec.ConversationID, limit, ownerValue)
	if err != nil {
		return nil, fmt.Errorf("list owner-scoped conversation payloads: %w", err)
	}
	defer rows.Close()
	var payloads []*capture.Payload
	for rows.Next() {
		var payload capture.Payload
		var reqCompressed, respCompressed []byte
		var reqKey, respKey string
		if err := rows.Scan(
			&payload.ID, &payload.RequestID, &payload.ConversationID,
			&payload.TenantID, &payload.UserID, &payload.KeyID,
			&payload.Provider, &payload.Model, &payload.Status, &payload.ErrorClass, &payload.Streaming,
			&reqCompressed, &respCompressed,
			&payload.RequestTruncated, &payload.ResponseTruncated, &payload.ResponseFormat,
			&payload.CreatedAt, &payload.CompletedAt, &payload.ExpiresAt,
			&reqKey, &respKey,
		); err != nil {
			return nil, fmt.Errorf("scan owner-scoped conversation payload: %w", err)
		}
		if err := s.hydrateBodies(ctx, &payload, reqCompressed, respCompressed, reqKey, respKey); err != nil {
			return nil, err
		}
		payloads = append(payloads, &payload)
	}
	return payloads, rows.Err()
}

// DeleteExpired removes payloads whose retention timestamp has passed. Rows
// whose bodies live in object storage are archival and never deleted here.
func (s *ConversationPayloadStore) DeleteExpired(ctx context.Context, now time.Time) (int64, error) {
	// Object-backed raw capture is a permanent archive policy, including legacy
	// inline rows not yet migrated. Never let a TTL race their archival repair.
	if s.rawInObjects {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM conversation_payloads p
		 WHERE expires_at IS NOT NULL
		   AND expires_at <= $1
		   AND body_location = 'postgres'
		   AND (NULLIF(conversation_id, '') IS NULL OR EXISTS (
		       SELECT 1 FROM session_transcripts s JOIN session_turns t
		         ON t.transcript_id=s.id AND t.generation=s.generation
		        WHERE s.conversation_id=p.conversation_id
		          AND COALESCE(s.tenant_id, '')=COALESCE(p.tenant_id, '')
		          AND NOT s.needs_rebuild AND t.request_id=p.request_id
		   ))
	`, now)
	if err != nil {
		return 0, fmt.Errorf("delete expired conversation payloads: %w", err)
	}
	return tag.RowsAffected(), nil
}

// PayloadSummary is a lightweight metadata view without decompressed bodies.
type PayloadSummary struct {
	ID              string     `json:"id"`
	RequestID       string     `json:"request_id"`
	ConversationID  string     `json:"conversation_id,omitempty"`
	TenantID        string     `json:"tenant_id,omitempty"`
	TenantName      string     `json:"tenant_name,omitempty"`
	UserID          string     `json:"user_id,omitempty"`
	UserDisplayName string     `json:"user_display_name,omitempty"`
	UserEmail       string     `json:"user_email,omitempty"`
	KeyID           string     `json:"key_id,omitempty"`
	Provider        string     `json:"provider"`
	Model           string     `json:"model"`
	Status          string     `json:"status"`
	ErrorClass      string     `json:"error_class,omitempty"`
	Streaming       bool       `json:"streaming"`
	ResponseFormat  string     `json:"response_format"`
	CreatedAt       time.Time  `json:"created_at"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
	LastPromptRole  string     `json:"last_prompt_role,omitempty"`
	LastPromptText  string     `json:"last_prompt_text,omitempty"`
	CostCents       float64    `json:"cost_total_cents,omitempty"`
}

// SessionSummary is a lightweight session view grouped by conversation_id or user.
// It is the pagination unit for the session list view — each row is one logical
// chat session, not an individual request payload.
type SessionSummary struct {
	OwnerID          string `json:"owner_id"`
	SuccessCount     int    `json:"success_count"`
	FailureCount     int    `json:"failure_count"`
	InterruptedCount int    `json:"interrupted_count"`
	UploadState      string `json:"upload_state"`
	TranscriptState  string `json:"transcript_state"`

	ConversationID  string    `json:"conversation_id,omitempty"`
	TenantID        string    `json:"tenant_id,omitempty"`
	UserID          string    `json:"user_id,omitempty"`
	TenantName      string    `json:"tenant_name,omitempty"`
	UserDisplayName string    `json:"user_display_name,omitempty"`
	UserEmail       string    `json:"user_email,omitempty"`
	Provider        string    `json:"provider"`
	Model           string    `json:"model"`
	Status          string    `json:"status"`
	MessageCount    int       `json:"message_count"`
	CostCents       float64   `json:"cost_total_cents"`
	StartedAt       time.Time `json:"started_at"`
	EndedAt         time.Time `json:"ended_at"`
	LastPromptRole  string    `json:"last_prompt_role,omitempty"`
	LastPromptText  string    `json:"last_prompt_text,omitempty"`
}

// PayloadSessionScope identifies one captured session, including anonymous owners.
type PayloadSessionScope struct {
	ConversationID string    `json:"conversation_id"`
	TenantID       string    `json:"tenant_id"`
	UserID         string    `json:"user_id"`
	OwnerID        string    `json:"owner_id"`
	From           time.Time `json:"from"`
	To             time.Time `json:"to"`
}

// List returns recent payload summaries (metadata only, no decompressed bodies).
// Filters by optional requestID, tenantID, userID, or provider. Ordered by created_at DESC.
// Returns the matching summaries, the total count of matching rows (not just this page),
// and any error. offset must be >= 0; limit is capped at 100.
func (s *ConversationPayloadStore) List(ctx context.Context, requestID, conversationID, tenantID, userID, provider string, limit, offset int, includePrompts bool, scopes ...PayloadSessionScope) (total int, summaries []*PayloadSummary, err error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	where := "WHERE 1=1"
	args := []interface{}{}
	argIdx := 1

	if requestID != "" {
		where += fmt.Sprintf(" AND cp.request_id = $%d", argIdx)
		args = append(args, requestID)
		argIdx++
	}
	if conversationID != "" {
		where += fmt.Sprintf(" AND cp.conversation_id = $%d", argIdx)
		args = append(args, conversationID)
		argIdx++
	}
	if tenantID != "" {
		where += fmt.Sprintf(" AND cp.tenant_id = $%d", argIdx)
		args = append(args, tenantID)
		argIdx++
	}
	if userID != "" {
		where += fmt.Sprintf(" AND cp.user_id = $%d", argIdx)
		args = append(args, userID)
		argIdx++
	}
	if provider != "" {
		where += fmt.Sprintf(" AND cp.provider = $%d", argIdx)
		args = append(args, provider)
		argIdx++
	}

	if len(scopes) > 0 {
		scope := scopes[0]
		owner := scope.OwnerID
		if owner == "" && scope.UserID != "" {
			owner = "user:" + scope.UserID
		}
		where += fmt.Sprintf(` AND COALESCE(cp.conversation_id, '') = $%d
   AND COALESCE(cp.tenant_id, '') = $%d
   AND (CASE WHEN NULLIF(cp.user_id, '') IS NOT NULL THEN 'user:' || cp.user_id
        WHEN NULLIF(cp.key_id, '') IS NOT NULL THEN 'key:' || cp.key_id ELSE '' END) = $%d
   AND cp.created_at >= $%d AND cp.created_at <= $%d`, argIdx, argIdx+1, argIdx+2, argIdx+3, argIdx+4)
		args = append(args, scope.ConversationID, scope.TenantID, owner, scope.From, scope.To)
		argIdx += 5
	}

	// Count query — same filters, no joins needed for count.
	countArgs := make([]interface{}, len(args))
	copy(countArgs, args)
	countQuery := fmt.Sprintf(`SELECT COUNT(*) FROM conversation_payloads cp %s`, where)
	if err := s.pool.QueryRow(ctx, countQuery, countArgs...).Scan(&total); err != nil {
		return 0, nil, fmt.Errorf("count conversation payloads: %w", err)
	}

	requestBodyExpr := "NULL::bytea"
	if includePrompts {
		requestBodyExpr = "cp.request_body_compressed"
	}

	query := fmt.Sprintf(`
		SELECT cp.id, cp.request_id, COALESCE(cp.conversation_id, ''),
		       COALESCE(cp.tenant_id, ''), COALESCE(t.name, ''),
		       COALESCE(cp.user_id, ''), COALESCE(u.display_name, ''), COALESCE(u.email, ''),
		       COALESCE(cp.key_id, ''),
		       cp.provider, cp.model, cp.status, COALESCE(cp.error_class, ''), cp.streaming,
		       cp.response_format, cp.created_at, cp.completed_at, cp.expires_at,
		       COALESCE(ue.cost_total_cents, 0),
		       %s
		  FROM conversation_payloads cp
		  LEFT JOIN tenants t ON t.id = cp.tenant_id
		  LEFT JOIN users u ON u.id = cp.user_id
		  LEFT JOIN usage_events ue ON ue.request_id = cp.request_id
		%s
		ORDER BY cp.created_at DESC, cp.request_id DESC
		LIMIT $%d OFFSET $%d
	`, requestBodyExpr, where, argIdx, argIdx+1)
	args = append(args, limit, offset)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return 0, nil, fmt.Errorf("list conversation payloads: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		s := &PayloadSummary{}
		var reqCompressed []byte
		if err := rows.Scan(&s.ID, &s.RequestID, &s.ConversationID,
			&s.TenantID, &s.TenantName,
			&s.UserID, &s.UserDisplayName, &s.UserEmail, &s.KeyID,
			&s.Provider, &s.Model, &s.Status, &s.ErrorClass, &s.Streaming,
			&s.ResponseFormat, &s.CreatedAt, &s.CompletedAt, &s.ExpiresAt,
			&s.CostCents,
			&reqCompressed); err != nil {
			return 0, nil, fmt.Errorf("scan payload summary: %w", err)
		}
		if includePrompts && len(reqCompressed) > 0 {
			body, err := capture.Decompress(reqCompressed)
			if err != nil {
				return 0, nil, fmt.Errorf("decompress request prompt summary: %w", err)
			}
			s.LastPromptRole, s.LastPromptText = extractLastPrompt(body)
		}
		summaries = append(summaries, s)
	}
	if summaries == nil {
		summaries = []*PayloadSummary{}
	}
	return total, summaries, rows.Err()
}

// ListSessions groups conversation payloads by conversation_id (when present) or
// tenant+user (fallback), split by idle time, and returns paginated session
// summaries. Each row represents one logical chat session with aggregate
// metadata.
// Filters by optional tenantID, userID, or provider. Ordered by ended_at DESC.
// offset must be >= 0; limit is capped at 100.
func (s *ConversationPayloadStore) ListSessions(ctx context.Context, tenantID, userID, provider string, limit, offset int) (total int, sessions []*SessionSummary, err error) {
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
		where += fmt.Sprintf(" AND cp.tenant_id = $%d", argIdx)
		args = append(args, tenantID)
		argIdx++
	}
	if userID != "" {
		where += fmt.Sprintf(" AND cp.user_id = $%d", argIdx)
		args = append(args, userID)
		argIdx++
	}
	if provider != "" {
		where += fmt.Sprintf(" AND cp.provider = $%d", argIdx)
		args = append(args, provider)
		argIdx++
	}

	const sessionGap = 30 * time.Minute
	gapSeconds := int(sessionGap.Seconds())

	countQuery := fmt.Sprintf(`
		WITH base AS (
			SELECT COALESCE(NULLIF(cp.conversation_id, ''), '') AS conversation_id,
			       COALESCE(cp.tenant_id, '') AS tenant_id,
			       COALESCE(cp.user_id, '') AS user_id,
			       COALESCE(cp.tenant_id, '') || '::' || COALESCE(NULLIF(cp.user_id, ''), 'key:' || cp.key_id, '') || '::' || COALESCE(NULLIF(cp.conversation_id, ''), 'no-conversation') AS group_key,
			       cp.created_at
			  FROM conversation_payloads cp
			%s
		),
		marked AS (
			SELECT *,
			       CASE
			         WHEN LAG(created_at) OVER (PARTITION BY group_key ORDER BY created_at) IS NULL THEN 1
			         WHEN created_at - LAG(created_at) OVER (PARTITION BY group_key ORDER BY created_at) > ($%d::int * interval '1 second') THEN 1
			         ELSE 0
			       END AS starts_session
			  FROM base
		),
		sessionized AS (
			SELECT *,
			       SUM(starts_session) OVER (PARTITION BY group_key ORDER BY created_at) AS session_num
			  FROM marked
		)
		SELECT COUNT(*) FROM (
			SELECT group_key, session_num
			  FROM sessionized
			 GROUP BY group_key, session_num
		) sub
	`, where, argIdx)
	countArgs := append([]interface{}{}, args...)
	countArgs = append(countArgs, gapSeconds)
	if err := s.pool.QueryRow(ctx, countQuery, countArgs...).Scan(&total); err != nil {
		return 0, nil, fmt.Errorf("count conversation sessions: %w", err)
	}

	query := conversationSessionListQuery(where, argIdx)
	args = append(args, gapSeconds, limit, offset)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return 0, nil, fmt.Errorf("list conversation sessions: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		sess := &SessionSummary{}
		var lastRequestBodyCompressed []byte
		if err := rows.Scan(
			&sess.ConversationID, &sess.TenantID, &sess.UserID,
			&sess.TenantName, &sess.UserDisplayName, &sess.UserEmail,
			&sess.MessageCount, &sess.StartedAt, &sess.EndedAt,
			&sess.Provider, &sess.Model, &sess.Status, &sess.CostCents,
			&lastRequestBodyCompressed,
			&sess.SuccessCount, &sess.FailureCount, &sess.InterruptedCount,
			&sess.UploadState, &sess.TranscriptState, &sess.LastPromptText, &sess.OwnerID,
		); err != nil {
			return 0, nil, fmt.Errorf("scan session summary: %w", err)
		}
		if sess.LastPromptText == "" && len(lastRequestBodyCompressed) > 0 {
			body, err := capture.Decompress(lastRequestBodyCompressed)
			if err != nil {
				return 0, nil, fmt.Errorf("decompress session last prompt summary: %w", err)
			}
			sess.LastPromptRole, sess.LastPromptText = extractLastPrompt(body)
		}
		sessions = append(sessions, sess)
	}
	if sessions == nil {
		sessions = []*SessionSummary{}
	}
	return total, sessions, rows.Err()
}

// conversationSessionListQuery keeps compressed payload bodies out of the
// full-table sessionization and aggregation stages. Fetching the latest body
// only after pagination prevents the session list from reading the table's
// entire TOAST payload corpus merely to render one page of prompt previews.
func conversationSessionListQuery(where string, argIdx int) string {
	return fmt.Sprintf(`
		WITH base AS (
			SELECT COALESCE(NULLIF(cp.conversation_id, ''), '') AS conversation_id,
			       COALESCE(cp.tenant_id, '') AS tenant_id,
			       COALESCE(cp.user_id, '') AS user_id,
			       COALESCE(cp.tenant_id, '') || '::' || COALESCE(NULLIF(cp.user_id, ''), 'key:' || cp.key_id, '') || '::' || COALESCE(NULLIF(cp.conversation_id, ''), 'no-conversation') AS group_key,
			       cp.request_id, cp.provider, cp.model, cp.status, cp.error_class, cp.created_at,
			       CASE WHEN NULLIF(cp.user_id, '') IS NOT NULL THEN 'user:' || cp.user_id
			            WHEN NULLIF(cp.key_id, '') IS NOT NULL THEN 'key:' || cp.key_id ELSE '' END AS owner_id,
			       COALESCE(ue.cost_total_cents, 0) AS cost_total_cents
			  FROM conversation_payloads cp
			  LEFT JOIN usage_events ue ON ue.request_id = cp.request_id
			%s
		),
		marked AS (
			SELECT *,
			       CASE
			         WHEN LAG(created_at) OVER (PARTITION BY group_key ORDER BY created_at) IS NULL THEN 1
			         WHEN created_at - LAG(created_at) OVER (PARTITION BY group_key ORDER BY created_at) > ($%d::int * interval '1 second') THEN 1
			         ELSE 0
			       END AS starts_session
			  FROM base
		),
		sessionized AS (
			SELECT *,
			       SUM(starts_session) OVER (PARTITION BY group_key ORDER BY created_at) AS session_num
			  FROM marked
		),
		aggregated AS (
			SELECT COALESCE(MAX(NULLIF(conversation_id, '')), '') AS conversation_id,
		       MAX(tenant_id) AS tenant_id,
		       MAX(user_id) AS user_id,
		       MAX(owner_id) AS owner_id,
		       COUNT(*) AS message_count,
		       MIN(created_at) AS started_at,
		       MAX(created_at) AS ended_at,
		       COALESCE((ARRAY_AGG(provider ORDER BY created_at DESC))[1], '') AS provider,
		       COALESCE((ARRAY_AGG(model ORDER BY created_at DESC))[1], '') AS model,
		       (ARRAY_AGG(CASE WHEN error_class = 'client_closed' OR status = 'cancelled'
		                            THEN 'cancelled' ELSE status END ORDER BY created_at DESC, request_id DESC))[1] AS status,
		       COUNT(*) FILTER (WHERE status = 'success') AS success_count,
		       COUNT(*) FILTER (WHERE status = 'error' AND COALESCE(error_class, '') <> 'client_closed') AS failure_count,
		       COUNT(*) FILTER (WHERE error_class = 'client_closed' OR status = 'cancelled') AS interrupted_count,
		       COALESCE(SUM(cost_total_cents), 0) AS cost_total_cents,
		       (ARRAY_AGG(request_id ORDER BY created_at DESC, request_id DESC))[1] AS last_request_id
		  FROM sessionized
		 GROUP BY group_key, session_num
		),
		paged AS MATERIALIZED (
			SELECT *
			  FROM aggregated
			 ORDER BY ended_at DESC
			 LIMIT $%d OFFSET $%d
		)
		SELECT p.conversation_id, p.tenant_id, p.user_id,
		       COALESCE(t.name, '') AS tenant_name,
		       COALESCE(u.display_name, '') AS user_display_name,
		       COALESCE(u.email, '') AS user_email,
		       p.message_count, p.started_at, p.ended_at,
		       p.provider, p.model, p.status, p.cost_total_cents,
		       latest_cp.request_body_compressed AS last_request_body_compressed,
		       p.success_count, p.failure_count, p.interrupted_count,
		       CASE WHEN pl.id IS NOT NULL THEN 'uploaded'
		            WHEN ss.id IS NOT NULL THEN 'pending' ELSE 'not_uploaded' END AS upload_state,
		       CASE WHEN tr.id IS NULL THEN 'not_available'
		            WHEN tr.needs_rebuild THEN 'rebuilding'
		            WHEN turn.request_id IS NULL THEN 'updating'
		            ELSE 'available' END AS transcript_state,
		       COALESCE(NULLIF(turn.prompt_preview, ''), ss.first_prompt, '') AS preview, p.owner_id
		  FROM paged p
		  LEFT JOIN tenants t ON t.id = NULLIF(p.tenant_id, '')
		  LEFT JOIN users u ON u.id = NULLIF(p.user_id, '')
		  LEFT JOIN conversation_payloads latest_cp ON latest_cp.request_id = p.last_request_id
		  LEFT JOIN session_submissions ss ON ss.tenant_id = NULLIF(p.tenant_id, '')
		       AND ss.owner_id = NULLIF(p.owner_id, '') AND ss.conversation_id = p.conversation_id
		  LEFT JOIN pi_session_logs pl ON pl.tenant_id = NULLIF(p.tenant_id, '')
		       AND pl.owner_id = NULLIF(p.owner_id, '') AND pl.session_id = p.conversation_id
		  LEFT JOIN session_transcripts tr ON COALESCE(tr.tenant_id, '') = p.tenant_id
		       AND tr.conversation_id = NULLIF(p.conversation_id, '')
		  LEFT JOIN session_turns turn ON turn.transcript_id = tr.id AND turn.generation = tr.generation
		       AND turn.request_id = p.last_request_id
		 ORDER BY p.ended_at DESC
	`, where, argIdx, argIdx+1, argIdx+2)
}

type promptRequest struct {
	Messages []promptMessage `json:"messages"`
	Input    interface{}     `json:"input"`
}

type promptMessage struct {
	Type    string      `json:"type"`
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
	Text    interface{} `json:"text"`
	Output  interface{} `json:"output"`
}

func extractLastPrompt(body []byte) (string, string) {
	var req promptRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return "", ""
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		msg := req.Messages[i]
		role := strings.TrimSpace(msg.Role)
		if role == "" || role == "system" {
			continue
		}
		text := strings.TrimSpace(promptContentText(msg.Content))
		if text != "" {
			return role, truncatePromptSummary(text)
		}
	}

	// The Responses API uses input instead of messages. Codex requests often
	// contain the complete conversation here, interleaved with reasoning and
	// function-call items, so walk backwards to find the turn that triggered
	// this request.
	switch input := req.Input.(type) {
	case string:
		if text := strings.TrimSpace(input); text != "" {
			return "user", truncatePromptSummary(text)
		}
	case []interface{}:
		for i := len(input) - 1; i >= 0; i-- {
			switch item := input[i].(type) {
			case string:
				if text := strings.TrimSpace(item); text != "" {
					return "user", truncatePromptSummary(text)
				}
			case map[string]interface{}:
				msg := promptMessage{
					Type:    stringValue(item["type"]),
					Role:    stringValue(item["role"]),
					Content: item["content"],
					Text:    item["text"],
					Output:  item["output"],
				}
				if role, text := promptMessageSummary(msg); text != "" {
					return role, truncatePromptSummary(text)
				}
			}
		}
	}
	return "", ""
}

func promptMessageSummary(msg promptMessage) (string, string) {
	role := strings.TrimSpace(msg.Role)
	if role == "system" || role == "developer" {
		return "", ""
	}

	text := strings.TrimSpace(promptContentText(msg.Content))
	if text == "" {
		text = strings.TrimSpace(promptContentText(msg.Text))
	}
	if text == "" && msg.Type == "function_call_output" {
		text = strings.TrimSpace(promptContentText(msg.Output))
	}
	if text == "" {
		return "", ""
	}

	if role == "" {
		switch msg.Type {
		case "function_call_output":
			role = "tool"
		case "input_text":
			role = "user"
		default:
			role = "message"
		}
	}
	return role, text
}

func stringValue(value interface{}) string {
	text, _ := value.(string)
	return text
}

func truncatePromptSummary(text string) string {
	runes := []rune(text)
	if len(runes) <= maxPromptSummaryRunes {
		return text
	}
	return string(runes[:maxPromptSummaryRunes]) + "\n[truncated]"
}

func promptContentText(content interface{}) string {
	switch v := content.(type) {
	case string:
		return v
	case []interface{}:
		parts := make([]string, 0, len(v))
		for _, raw := range v {
			switch part := raw.(type) {
			case string:
				if strings.TrimSpace(part) != "" {
					parts = append(parts, part)
				}
			case map[string]interface{}:
				if text := strings.TrimSpace(promptContentText(part)); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	case map[string]interface{}:
		for _, key := range []string{"text", "input_text", "output_text", "content"} {
			if text := strings.TrimSpace(promptContentText(v[key])); text != "" {
				return text
			}
		}
		return ""
	default:
		return ""
	}
}

func conversationPayloadArgs(payload *capture.Payload, bodies storedBodies) []interface{} {
	return []interface{}{
		payload.ID,
		payload.RequestID,
		nullableText(payload.ConversationID),
		nullableText(payload.TenantID),
		nullableText(payload.UserID),
		nullableText(payload.KeyID),
		payload.Provider,
		payload.Model,
		payload.Status,
		nullableText(payload.ErrorClass),
		payload.Streaming,
		bodies.request,
		bodies.response,
		payload.RequestTruncated,
		payload.ResponseTruncated,
		payload.ResponseFormat,
		payload.CreatedAt,
		payload.CompletedAt,
		payload.ExpiresAt,
		bodies.location,
		nullableText(bodies.requestKey),
		nullableText(bodies.responseKey),
		int64(len(payload.RequestBody)),
		int64(len(payload.ResponseBody)),
	}
}
