package postgres

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/earlgrey/piproxy/internal/blob"
	"github.com/earlgrey/piproxy/internal/id"
	"github.com/earlgrey/piproxy/internal/sessionarchive"
)

// ErrSessionLogNotFound indicates no canonical Pi session log exists for the
// requested owner-scoped session id.
var ErrSessionLogNotFound = errors.New("pi session log not found")

// PiSessionLogRecord is one canonical gzip-compressed Pi session JSONL file.
type PiSessionLogRecord struct {
	ID                string
	TenantID          string
	UserID            string
	KeyID             string
	OwnerID           string
	SessionID         string
	SessionFileName   string
	Project           string
	ProjectID         string
	CWD               string
	FormatVersion     int
	ParentSession     string
	ContentSHA256     string
	UncompressedBytes int64
	EntryCount        int
	ContentGzip       []byte
	SessionCreatedAt  *time.Time
	SessionModifiedAt *time.Time
}

// PiSessionLogStore persists canonical Pi session logs: metadata in Postgres,
// the gzip JSONL in content_gzip and/or object storage.
type PiSessionLogStore struct {
	pool          *Pool
	objects       blob.Objects
	rawInObjects  bool
	rawInPostgres bool
}

func NewPiSessionLogStore(pool *Pool) *PiSessionLogStore {
	return &PiSessionLogStore{pool: pool, rawInPostgres: true}
}

// WithObjects attaches object storage. Reads follow each row's body_location;
// inObjects/inPostgres choose where new uploads are written. Object-backed
// exports are archival: retention never deletes them.
func (s *PiSessionLogStore) WithObjects(objects blob.Objects, inObjects, inPostgres bool) *PiSessionLogStore {
	s.objects = objects
	s.rawInObjects = inObjects && objects != nil
	s.rawInPostgres = inPostgres || !s.rawInObjects
	return s
}

// ExistingHashes returns the current content hash for the requested session
// ids, scoped to the authenticated owner.
func (s *PiSessionLogStore) ExistingHashes(ctx context.Context, tenantID, ownerID string, sessionIDs []string) (map[string]string, error) {
	out := make(map[string]string, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return out, nil
	}

	rows, err := s.pool.Query(ctx, `
		SELECT session_id, content_sha256
		  FROM pi_session_logs
		 WHERE tenant_id = $1
		   AND owner_id = $2
		   AND session_id = ANY($3)
	`, tenantID, ownerID, sessionIDs)
	if err != nil {
		return nil, fmt.Errorf("list existing Pi session hashes: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var sessionID, hash string
		if err := rows.Scan(&sessionID, &hash); err != nil {
			return nil, fmt.Errorf("scan existing Pi session hash: %w", err)
		}
		out[sessionID] = hash
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate existing Pi session hashes: %w", err)
	}
	return out, nil
}

// Put inserts or replaces the current content for a Pi session. Session files
// are append-oriented, so the same session id may legitimately acquire a new
// hash as more entries are written. Object keys are content-addressed; the
// superseded object is deleted only after the new row commits.
func (s *PiSessionLogStore) Put(ctx context.Context, record *PiSessionLogRecord) error {
	if record.ID == "" {
		record.ID = id.New("pilog")
	}

	location := BodyLocationPostgres
	content := record.ContentGzip
	var objectKey string
	if s.rawInObjects {
		objectKey = sessionarchive.PiSessionKey(record.TenantID, record.OwnerID, record.SessionID, record.ContentSHA256)
		if err := s.objects.Put(ctx, objectKey, record.ContentGzip, gzipContentType); err != nil {
			return fmt.Errorf("store Pi session log object: %w", err)
		}
		location = s.objects.Location()
		if !s.rawInPostgres {
			content = nil
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin Pi session log upsert: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var previousKey string
	err = tx.QueryRow(ctx, `
		SELECT COALESCE(object_key, '') FROM pi_session_logs
		 WHERE tenant_id = $1 AND owner_id = $2 AND session_id = $3
		 FOR UPDATE
	`, record.TenantID, record.OwnerID, record.SessionID).Scan(&previousKey)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read previous Pi session log: %w", err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO pi_session_logs
			(id, tenant_id, user_id, key_id, owner_id, session_id,
			 session_file_name, project, project_id, cwd, format_version, parent_session,
			 content_sha256, uncompressed_bytes, entry_count, content_gzip,
			 session_created_at, session_modified_at, uploaded_at,
			 body_location, object_key)
		VALUES
			($1, $2, $3, $4, $5, $6,
			 $7, $8, $9, $10, $11, $12,
			 $13, $14, $15, $16,
			 $17, $18, now(),
			 $19, $20)
		ON CONFLICT (tenant_id, owner_id, session_id) DO UPDATE SET
			body_location = EXCLUDED.body_location,
			object_key = EXCLUDED.object_key,
			user_id = EXCLUDED.user_id,
			key_id = EXCLUDED.key_id,
			session_file_name = EXCLUDED.session_file_name,
			project = EXCLUDED.project,
			project_id = COALESCE(EXCLUDED.project_id, pi_session_logs.project_id),
			cwd = EXCLUDED.cwd,
			format_version = EXCLUDED.format_version,
			parent_session = EXCLUDED.parent_session,
			content_sha256 = EXCLUDED.content_sha256,
			uncompressed_bytes = EXCLUDED.uncompressed_bytes,
			entry_count = EXCLUDED.entry_count,
			content_gzip = EXCLUDED.content_gzip,
			session_created_at = EXCLUDED.session_created_at,
			session_modified_at = EXCLUDED.session_modified_at,
			uploaded_at = now()
	`, record.ID, record.TenantID, nullableText(record.UserID), nullableText(record.KeyID), record.OwnerID, record.SessionID,
		nullableText(record.SessionFileName), nullableText(record.Project), nullableText(record.ProjectID), nullableText(record.CWD), record.FormatVersion, nullableText(record.ParentSession),
		record.ContentSHA256, record.UncompressedBytes, record.EntryCount, content,
		record.SessionCreatedAt, record.SessionModifiedAt,
		location, nullableText(objectKey))
	if err != nil {
		return fmt.Errorf("upsert Pi session log: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit Pi session log: %w", err)
	}
	if previousKey != "" && previousKey != objectKey && s.objects != nil {
		// Best-effort: an orphaned superseded export wastes space but loses nothing.
		_ = s.objects.Delete(context.WithoutCancel(ctx), previousKey)
	}
	return nil
}

// GetBySession returns one owner-scoped canonical Pi session log together with
// its gzip-decompressed JSONL content. It returns ErrSessionLogNotFound when no
// row matches so callers can distinguish "nothing to distill" from a real fault.
func (s *PiSessionLogStore) GetBySession(ctx context.Context, tenantID, ownerID, sessionID string) (*PiSessionLogRecord, []byte, error) {
	r := &PiSessionLogRecord{}
	var compressed []byte
	var objectKey string
	err := s.pool.QueryRow(ctx, `
		SELECT id, tenant_id, COALESCE(user_id, ''), COALESCE(key_id, ''), owner_id,
		       session_id, COALESCE(session_file_name, ''), COALESCE(project, ''),
		       COALESCE(project_id, ''), COALESCE(cwd, ''), COALESCE(format_version, 0),
		       COALESCE(parent_session, ''), content_sha256, uncompressed_bytes,
		       entry_count, content_gzip, session_created_at, session_modified_at,
		       COALESCE(object_key, '')
		  FROM pi_session_logs
		 WHERE tenant_id = $1 AND owner_id = $2 AND session_id = $3
		 LIMIT 1
	`, tenantID, ownerID, sessionID).Scan(
		&r.ID, &r.TenantID, &r.UserID, &r.KeyID, &r.OwnerID,
		&r.SessionID, &r.SessionFileName, &r.Project,
		&r.ProjectID, &r.CWD, &r.FormatVersion,
		&r.ParentSession, &r.ContentSHA256, &r.UncompressedBytes,
		&r.EntryCount, &compressed, &r.SessionCreatedAt, &r.SessionModifiedAt,
		&objectKey,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, ErrSessionLogNotFound
		}
		return nil, nil, fmt.Errorf("get owner-scoped Pi session log: %w", err)
	}
	if compressed == nil && objectKey != "" {
		if s.objects == nil {
			return nil, nil, fmt.Errorf("Pi session log %s is in object storage, which is not configured", sessionID)
		}
		if compressed, err = s.objects.Read(ctx, objectKey); err != nil {
			return nil, nil, fmt.Errorf("read Pi session log object: %w", err)
		}
	}

	raw, err := gunzipSessionLog(compressed)
	if err != nil {
		return nil, nil, fmt.Errorf("decompress Pi session log %s: %w", sessionID, err)
	}
	return r, raw, nil
}

func gunzipSessionLog(compressed []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

// DeleteOlderThan removes raw canonical session files after the configured
// retention window. Metadata summaries and usage records are unaffected, and
// object-backed exports are archival and never deleted here.
func (s *PiSessionLogStore) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM pi_session_logs WHERE uploaded_at < $1 AND body_location = 'postgres'`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("delete expired Pi session logs: %w", err)
	}
	return tag.RowsAffected(), nil
}
