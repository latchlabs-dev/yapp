package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/earlgrey/piproxy/internal/id"
)

// ErrAttachmentNotFound covers missing, wrong-tenant, and wrong-owner
// attachments alike, so an ID from another tenant is indistinguishable from a
// nonexistent one.
var ErrAttachmentNotFound = errors.New("attachment not found")

var ErrAttachmentBudgetExceeded = errors.New("attachment storage budget exceeded")
var ErrAttachmentOwnerInactive = errors.New("attachment owner is inactive")

// AttachmentLimits count reserved and stored bytes until cleanup actually
// removes them. These are independent of LLM spend and apply across replicas.
type AttachmentLimits struct {
	UserBytes      int64
	TenantBytes    int64
	PendingPerUser int64
}

func (l AttachmentLimits) withDefaults() AttachmentLimits {
	if l.UserBytes <= 0 {
		l.UserBytes = 1 << 30
	}
	if l.TenantBytes <= 0 {
		l.TenantBytes = 10 << 30
	}
	if l.PendingPerUser <= 0 {
		l.PendingPerUser = 16
	}
	return l
}

// Attachment lifecycle states. A row is created `pending` when the signed
// upload URL is issued and only becomes `ready` once the stored object has
// been verified, so an abandoned upload is never attached to a message.
const (
	AttachmentPending  = "pending"
	AttachmentReady    = "ready"
	AttachmentRejected = "rejected"
)

// WebAttachment is metadata for one uploaded file. The bytes live in object
// storage; nothing here holds content.
type WebAttachment struct {
	ID              string     `json:"id"`
	TenantID        string     `json:"-"`
	UserID          string     `json:"-"`
	ConversationID  string     `json:"conversation_id,omitempty"`
	MessageID       string     `json:"message_id,omitempty"`
	Filename        string     `json:"filename"`
	MimeType        string     `json:"mime_type"`
	Kind            string     `json:"kind"`
	SizeBytes       int64      `json:"size_bytes"`
	SHA256          string     `json:"sha256,omitempty"`
	Bucket          string     `json:"-"`
	ObjectKey       string     `json:"-"`
	Status          string     `json:"status"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	FinalizedAt     *time.Time `json:"finalized_at,omitempty"`
	UploadExpiresAt time.Time  `json:"-"`
}

// WebAttachmentStore persists attachment metadata.
type WebAttachmentStore struct {
	pool *Pool
}

// NewWebAttachmentStore creates a new WebAttachmentStore.
func NewWebAttachmentStore(pool *Pool) *WebAttachmentStore {
	return &WebAttachmentStore{pool: pool}
}

// CreatePending records an attachment whose upload URL has just been issued.
func (s *WebAttachmentStore) CreatePending(ctx context.Context, a *WebAttachment, limits AttachmentLimits) (*WebAttachment, error) {
	if a.SizeBytes <= 0 || !a.UploadExpiresAt.After(time.Now()) {
		return nil, fmt.Errorf("invalid attachment reservation size or expiry")
	}
	limits = limits.withDefaults()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin attachment reservation: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Lock the tenant before checking totals, so two processes cannot reserve
	// the same remaining capacity. The locks last only through this small INSERT.
	var owner string
	if err := tx.QueryRow(ctx, `SELECT id FROM tenants WHERE id = $1 AND status = 'active' FOR UPDATE`, a.TenantID).Scan(&owner); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAttachmentOwnerInactive
		}
		return nil, fmt.Errorf("lock attachment tenant: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id = $1 AND tenant_id = $2 AND status = 'active' FOR UPDATE`, a.UserID, a.TenantID).Scan(&owner); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAttachmentOwnerInactive
		}
		return nil, fmt.Errorf("lock attachment user: %w", err)
	}
	var tenantBytes, userBytes, pending int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(size_bytes), 0),
		COALESCE(SUM(size_bytes) FILTER (WHERE user_id = $2), 0),
		COUNT(*) FILTER (WHERE user_id = $2 AND status IN ('pending', 'rejected'))
		FROM web_attachments WHERE tenant_id = $1`, a.TenantID, a.UserID).Scan(&tenantBytes, &userBytes, &pending); err != nil {
		return nil, fmt.Errorf("read attachment storage usage: %w", err)
	}
	if a.SizeBytes > limits.TenantBytes-tenantBytes || a.SizeBytes > limits.UserBytes-userBytes || pending >= limits.PendingPerUser {
		return nil, ErrAttachmentBudgetExceeded
	}
	if a.ID == "" {
		a.ID = id.New("watt")
	}
	a.Status = AttachmentPending
	_, err = tx.Exec(ctx,
		`INSERT INTO web_attachments
		     (id, tenant_id, user_id, conversation_id, filename, mime_type, kind,
		      size_bytes, bucket, object_key, status, expires_at, upload_expires_at, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, now())`,
		a.ID, a.TenantID, a.UserID, nullableText(a.ConversationID), a.Filename, a.MimeType, a.Kind,
		a.SizeBytes, a.Bucket, a.ObjectKey, a.Status, a.ExpiresAt, a.UploadExpiresAt,
	)
	if err != nil {
		return nil, fmt.Errorf("create pending attachment: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit attachment reservation: %w", err)
	}
	return a, nil
}

// Reject retains the object and row until upload authority expires. Deleting an
// object while its create-only signed URL is live would let a replay recreate
// an untracked object after the row had gone.
func (s *WebAttachmentStore) Reject(ctx context.Context, tenantID, userID, attachmentID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE web_attachments SET status = $4
		WHERE id = $1 AND tenant_id = $2 AND user_id = $3 AND status = $5`,
		attachmentID, tenantID, userID, AttachmentRejected, AttachmentPending)
	if err != nil {
		return fmt.Errorf("reject attachment: %w", err)
	}
	return nil
}

// Finalize flips a pending attachment to ready with the verified size and
// sniffed MIME type. The size and type come from the stored object, never from
// what the client claimed at upload time.
func (s *WebAttachmentStore) Finalize(ctx context.Context, tenantID, userID, attachmentID, mimeType, kind, sha256 string, sizeBytes int64) (*WebAttachment, error) {
	a := &WebAttachment{}
	var conversationID, messageID, sha *string
	err := s.pool.QueryRow(ctx,
		`UPDATE web_attachments
		    SET status = $4, mime_type = $5, kind = $6, size_bytes = $7, sha256 = $8, finalized_at = now()
		  WHERE id = $1 AND tenant_id = $2 AND user_id = $3 AND status = $9
		  RETURNING id, tenant_id, user_id, conversation_id, message_id, filename, mime_type, kind,
		            size_bytes, sha256, bucket, object_key, status, expires_at, created_at, finalized_at`,
		attachmentID, tenantID, userID, AttachmentReady, mimeType, kind, sizeBytes,
		nullableText(sha256), AttachmentPending,
	).Scan(&a.ID, &a.TenantID, &a.UserID, &conversationID, &messageID, &a.Filename, &a.MimeType, &a.Kind,
		&a.SizeBytes, &sha, &a.Bucket, &a.ObjectKey, &a.Status, &a.ExpiresAt, &a.CreatedAt, &a.FinalizedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAttachmentNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("finalize attachment: %w", err)
	}
	applyAttachmentOptionals(a, conversationID, messageID, sha)
	return a, nil
}

// Get returns one attachment owned by the given user, in any status.
func (s *WebAttachmentStore) Get(ctx context.Context, tenantID, userID, attachmentID string) (*WebAttachment, error) {
	a := &WebAttachment{}
	var conversationID, messageID, sha *string
	err := s.pool.QueryRow(ctx,
		`SELECT id, tenant_id, user_id, conversation_id, message_id, filename, mime_type, kind,
		        size_bytes, sha256, bucket, object_key, status, expires_at, created_at, finalized_at
		   FROM web_attachments
		  WHERE id = $1 AND tenant_id = $2 AND user_id = $3`,
		attachmentID, tenantID, userID,
	).Scan(&a.ID, &a.TenantID, &a.UserID, &conversationID, &messageID, &a.Filename, &a.MimeType, &a.Kind,
		&a.SizeBytes, &sha, &a.Bucket, &a.ObjectKey, &a.Status, &a.ExpiresAt, &a.CreatedAt, &a.FinalizedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAttachmentNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get attachment: %w", err)
	}
	applyAttachmentOptionals(a, conversationID, messageID, sha)
	return a, nil
}

// GetReadyBatch returns the ready attachments among the given IDs that belong
// to the user. IDs that are missing, pending, or owned by someone else are
// simply absent from the result — the caller compares counts and refuses the
// turn rather than silently dropping a file the user thought they attached.
func (s *WebAttachmentStore) GetReadyBatch(ctx context.Context, tenantID, userID string, ids []string) ([]*WebAttachment, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, tenant_id, user_id, conversation_id, message_id, filename, mime_type, kind,
		        size_bytes, sha256, bucket, object_key, status, expires_at, created_at, finalized_at
		   FROM web_attachments
		  WHERE id = ANY($1) AND tenant_id = $2 AND user_id = $3 AND status = $4`,
		ids, tenantID, userID, AttachmentReady,
	)
	if err != nil {
		return nil, fmt.Errorf("get attachments: %w", err)
	}
	defer rows.Close()

	var result []*WebAttachment
	for rows.Next() {
		a := &WebAttachment{}
		var conversationID, messageID, sha *string
		if err := rows.Scan(&a.ID, &a.TenantID, &a.UserID, &conversationID, &messageID, &a.Filename,
			&a.MimeType, &a.Kind, &a.SizeBytes, &sha, &a.Bucket, &a.ObjectKey, &a.Status,
			&a.ExpiresAt, &a.CreatedAt, &a.FinalizedAt); err != nil {
			return nil, fmt.Errorf("scan attachment: %w", err)
		}
		applyAttachmentOptionals(a, conversationID, messageID, sha)
		result = append(result, a)
	}
	return result, rows.Err()
}

// AttachToMessage binds ready attachments to the message that carried them.
func (s *WebAttachmentStore) AttachToMessage(ctx context.Context, conversationID, messageID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE web_attachments SET message_id = $1, conversation_id = $2 WHERE id = ANY($3)`,
		messageID, conversationID, ids,
	)
	if err != nil {
		return fmt.Errorf("attach to message: %w", err)
	}
	return nil
}

// ListSweepable returns attachments whose objects should be deleted: expired
// rows, plus pending rows older than pendingTTL whose upload never completed.
func (s *WebAttachmentStore) ListSweepable(ctx context.Context, pendingTTL time.Duration, limit int) ([]*WebAttachment, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, tenant_id, user_id, conversation_id, message_id, filename, mime_type, kind,
		        size_bytes, sha256, bucket, object_key, status, expires_at, created_at, finalized_at
		   FROM web_attachments
		  WHERE ((expires_at IS NOT NULL AND expires_at < now())
		     OR status = 'rejected'
		     OR (status = $1 AND created_at < now() - make_interval(secs => $2::float8)))
		    AND COALESCE(upload_expires_at, created_at + INTERVAL '7 days') < now()
		  ORDER BY created_at, id
		  LIMIT $3`,
		AttachmentPending, pendingTTL.Seconds(), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list sweepable attachments: %w", err)
	}
	defer rows.Close()

	var result []*WebAttachment
	for rows.Next() {
		a := &WebAttachment{}
		var conversationID, messageID, sha *string
		if err := rows.Scan(&a.ID, &a.TenantID, &a.UserID, &conversationID, &messageID, &a.Filename,
			&a.MimeType, &a.Kind, &a.SizeBytes, &sha, &a.Bucket, &a.ObjectKey, &a.Status,
			&a.ExpiresAt, &a.CreatedAt, &a.FinalizedAt); err != nil {
			return nil, fmt.Errorf("scan attachment: %w", err)
		}
		applyAttachmentOptionals(a, conversationID, messageID, sha)
		result = append(result, a)
	}
	return result, rows.Err()
}

// Delete removes attachment rows by ID. The caller deletes the stored objects
// first, so a failure here leaves an orphaned row rather than an orphaned
// object that nothing will ever sweep again.
func (s *WebAttachmentStore) Delete(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM web_attachments WHERE id = ANY($1)`, ids)
	if err != nil {
		return fmt.Errorf("delete attachments: %w", err)
	}
	return nil
}

func applyAttachmentOptionals(a *WebAttachment, conversationID, messageID, sha *string) {
	if conversationID != nil {
		a.ConversationID = *conversationID
	}
	if messageID != nil {
		a.MessageID = *messageID
	}
	if sha != nil {
		a.SHA256 = *sha
	}
}
