package postgres

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/earlgrey/piproxy/internal/sessionarchive"
)

// ConversationRef identifies a conversation with captured requests.
type ConversationRef struct {
	TenantID       string
	ConversationID string
	LastPayloadAt  time.Time
	RequestCount   int
}

// BackfillFilter selects conversations for transcript backfill.
type BackfillFilter struct {
	// TenantID restricts to one tenant; nil means every tenant (an empty
	// string selects unauthenticated captures).
	TenantID *string
	Since    *time.Time
	// All includes conversations whose transcript is already current.
	All   bool
	Limit int
	// UnqueuedOnly is used by reconciliation; pending rows already form the queue.
	UnqueuedOnly    bool
	ObjectsLocation string
}

// ConversationsNeedingTranscripts lists conversations with captures but no
// transcript (or one flagged for rebuild), most recently active first.
func (s *SessionArchiveStore) ConversationsNeedingTranscripts(ctx context.Context, f BackfillFilter) ([]ConversationRef, error) {
	if f.Limit <= 0 {
		f.Limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT COALESCE(cp.tenant_id, ''), cp.conversation_id, MAX(cp.created_at), COUNT(*)
		  FROM conversation_payloads cp
		  LEFT JOIN session_transcripts st
		         ON COALESCE(st.tenant_id, '') = COALESCE(cp.tenant_id, '')
		        AND st.conversation_id = cp.conversation_id
		 WHERE cp.conversation_id IS NOT NULL AND cp.conversation_id <> ''
		   AND ($1::text IS NULL OR COALESCE(cp.tenant_id, '') = $1)
		   AND ($2::timestamptz IS NULL OR cp.created_at >= $2)
		   AND (NOT $5 OR st.id IS NULL OR NOT st.needs_rebuild)
		 GROUP BY 1, 2
		 HAVING $3 OR bool_or(st.id IS NULL OR st.needs_rebuild
		   OR ($6<>'' AND st.objects_location<>$6) OR (
		       COALESCE(cp.completed_at,cp.created_at) < now()-INTERVAL '5 minutes' AND NOT EXISTS (
		           SELECT 1 FROM session_turns t WHERE t.transcript_id=st.id
		             AND t.generation=st.generation AND t.request_id=cp.request_id)))
		 ORDER BY 3 DESC
		 LIMIT $4
	`, f.TenantID, f.Since, f.All, f.Limit, f.UnqueuedOnly, f.ObjectsLocation)
	if err != nil {
		return nil, fmt.Errorf("list conversations needing transcripts: %w", err)
	}
	defer rows.Close()
	var refs []ConversationRef
	for rows.Next() {
		var ref ConversationRef
		if err := rows.Scan(&ref.TenantID, &ref.ConversationID, &ref.LastPayloadAt, &ref.RequestCount); err != nil {
			return nil, fmt.Errorf("scan backfill conversation: %w", err)
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

// ConversationBodyBytes sums the compressed body bytes a conversation's
// captures hold in Postgres.
func (s *ConversationPayloadStore) ConversationBodyBytes(ctx context.Context, tenantID, conversationID string) (int64, error) {
	var total int64
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(COALESCE(octet_length(request_body_compressed), 0) + COALESCE(octet_length(response_body_compressed), 0)), 0)
		  FROM conversation_payloads
		 WHERE conversation_id = $2 AND COALESCE(tenant_id, '') = $1
	`, tenantID, conversationID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("sum conversation body bytes: %w", err)
	}
	return total, nil
}

// BodyMigrationOptions controls moving bodies from Postgres to objects.
type BodyMigrationOptions struct {
	// TenantID restricts the run to one tenant (pilot migrations, tests); nil
	// means every tenant and an empty string selects unauthenticated captures.
	TenantID    *string
	Batch       int
	MaxRows     int // 0 = no limit
	Concurrency int
	// DropPostgresBodies NULLs the bytea copy of rows already in object
	// storage. Without it rows keep both copies (a safety window).
	DropPostgresBodies bool
	DryRun             bool
}

// BodyMigrationStats reports a migration run.
type BodyMigrationStats struct {
	Pending int64 // rows still holding only Postgres bodies (dry run)
	Moved   int
	Bytes   int64
	Dropped int64
}

type pendingBody struct {
	id        string
	tenantID  string
	ownerID   string
	createdAt time.Time
}

// MigrateBodiesToObjects copies Postgres-held capture bodies to object
// storage byte-for-byte (they are already gzip), records the keys, and clears
// expires_at: object-backed captures are kept forever. With
// DropPostgresBodies it then NULLs the bytea copies of object-backed rows.
func (s *ConversationPayloadStore) MigrateBodiesToObjects(ctx context.Context, opts BodyMigrationOptions, progress func(BodyMigrationStats)) (BodyMigrationStats, error) {
	var stats BodyMigrationStats
	if s.objects == nil {
		return stats, fmt.Errorf("body migration requires object storage")
	}
	opts = normalizeMigrationOptions(opts)
	if opts.DryRun {
		err := s.pool.QueryRow(ctx, `
			SELECT count(*), COALESCE(SUM(octet_length(request_body_compressed) + octet_length(response_body_compressed)), 0)
			  FROM conversation_payloads
			 WHERE body_location = 'postgres' AND request_body_compressed IS NOT NULL
			   AND ($1::text IS NULL OR COALESCE(tenant_id, '') = $1)
		`, opts.TenantID).Scan(&stats.Pending, &stats.Bytes)
		return stats, err
	}

	location := s.objects.Location()
	err := migrateInBatches(ctx, opts, &stats, progress, func(ctx context.Context, limit int) ([]pendingBody, error) {
		rows, err := s.pool.Query(ctx, `
			SELECT request_id, COALESCE(tenant_id, ''), created_at
			  FROM conversation_payloads
			 WHERE body_location = 'postgres' AND request_body_compressed IS NOT NULL
			   AND ($2::text IS NULL OR COALESCE(tenant_id, '') = $2)
			 ORDER BY created_at, request_id
			 LIMIT $1
		`, limit, opts.TenantID)
		if err != nil {
			return nil, fmt.Errorf("select capture bodies to migrate: %w", err)
		}
		defer rows.Close()
		var batch []pendingBody
		for rows.Next() {
			var p pendingBody
			if err := rows.Scan(&p.id, &p.tenantID, &p.createdAt); err != nil {
				return nil, err
			}
			batch = append(batch, p)
		}
		return batch, rows.Err()
	}, func(ctx context.Context, p pendingBody) (int64, error) {
		var req, resp []byte
		if err := s.pool.QueryRow(ctx, `
			SELECT request_body_compressed, response_body_compressed
			  FROM conversation_payloads WHERE request_id = $1
		`, p.id).Scan(&req, &resp); err != nil {
			return 0, fmt.Errorf("read capture %s: %w", p.id, err)
		}
		reqKey := sessionarchive.CaptureRequestKey(p.tenantID, p.createdAt, p.id)
		respKey := sessionarchive.CaptureResponseKey(p.tenantID, p.createdAt, p.id)
		if err := s.objects.Put(ctx, reqKey, req, gzipContentType); err != nil {
			return 0, fmt.Errorf("store request body %s: %w", p.id, err)
		}
		if err := s.objects.Put(ctx, respKey, resp, gzipContentType); err != nil {
			return 0, fmt.Errorf("store response body %s: %w", p.id, err)
		}
		_, err := s.pool.Exec(ctx, `
			UPDATE conversation_payloads
			   SET body_location = $2, request_object_key = $3, response_object_key = $4, expires_at = NULL,
			       request_body_compressed = CASE WHEN $5 THEN NULL ELSE request_body_compressed END,
			       response_body_compressed = CASE WHEN $5 THEN NULL ELSE response_body_compressed END
			 WHERE request_id = $1 AND body_location = 'postgres'
		`, p.id, location, reqKey, respKey, opts.DropPostgresBodies)
		if err != nil {
			return 0, fmt.Errorf("record migrated capture %s: %w", p.id, err)
		}
		return int64(len(req) + len(resp)), nil
	})
	if err != nil || !opts.DropPostgresBodies {
		return stats, err
	}

	// Rows moved earlier without dropping still hold both copies.
	for {
		tag, err := s.pool.Exec(ctx, `
			UPDATE conversation_payloads
			   SET request_body_compressed = NULL, response_body_compressed = NULL
			 WHERE request_id IN (
				SELECT request_id FROM conversation_payloads
				 WHERE body_location <> 'postgres'
				   AND request_object_key IS NOT NULL AND response_object_key IS NOT NULL
				   AND (request_body_compressed IS NOT NULL OR response_body_compressed IS NOT NULL)
				   AND ($2::text IS NULL OR COALESCE(tenant_id, '') = $2)
				 LIMIT $1)
		`, opts.Batch, opts.TenantID)
		if err != nil {
			return stats, fmt.Errorf("drop migrated Postgres bodies: %w", err)
		}
		stats.Dropped += tag.RowsAffected()
		if progress != nil {
			progress(stats)
		}
		if tag.RowsAffected() == 0 {
			return stats, nil
		}
	}
}

// MigrateToObjects moves Postgres-held Pi session exports to object storage,
// following the same rules as MigrateBodiesToObjects.
func (s *PiSessionLogStore) MigrateToObjects(ctx context.Context, opts BodyMigrationOptions, progress func(BodyMigrationStats)) (BodyMigrationStats, error) {
	var stats BodyMigrationStats
	if s.objects == nil {
		return stats, fmt.Errorf("Pi session log migration requires object storage")
	}
	opts = normalizeMigrationOptions(opts)
	if opts.DryRun {
		err := s.pool.QueryRow(ctx, `
			SELECT count(*), COALESCE(SUM(octet_length(content_gzip)), 0)
			  FROM pi_session_logs WHERE body_location = 'postgres' AND content_gzip IS NOT NULL
			   AND ($1::text IS NULL OR tenant_id = $1)
		`, opts.TenantID).Scan(&stats.Pending, &stats.Bytes)
		return stats, err
	}

	location := s.objects.Location()
	return stats, migrateInBatches(ctx, opts, &stats, progress, func(ctx context.Context, limit int) ([]pendingBody, error) {
		rows, err := s.pool.Query(ctx, `
			SELECT id, tenant_id, owner_id, uploaded_at
			  FROM pi_session_logs
			 WHERE body_location = 'postgres' AND content_gzip IS NOT NULL
			   AND ($2::text IS NULL OR tenant_id = $2)
			 ORDER BY uploaded_at, id
			 LIMIT $1
		`, limit, opts.TenantID)
		if err != nil {
			return nil, fmt.Errorf("select Pi session logs to migrate: %w", err)
		}
		defer rows.Close()
		var batch []pendingBody
		for rows.Next() {
			var p pendingBody
			if err := rows.Scan(&p.id, &p.tenantID, &p.ownerID, &p.createdAt); err != nil {
				return nil, err
			}
			batch = append(batch, p)
		}
		return batch, rows.Err()
	}, func(ctx context.Context, p pendingBody) (int64, error) {
		var sessionID, sha string
		var content []byte
		if err := s.pool.QueryRow(ctx, `
			SELECT session_id, content_sha256, content_gzip FROM pi_session_logs WHERE id = $1
		`, p.id).Scan(&sessionID, &sha, &content); err != nil {
			return 0, fmt.Errorf("read Pi session log %s: %w", p.id, err)
		}
		key := sessionarchive.PiSessionKey(p.tenantID, p.ownerID, sessionID, sha)
		if err := s.objects.Put(ctx, key, content, gzipContentType); err != nil {
			return 0, fmt.Errorf("store Pi session log %s: %w", p.id, err)
		}
		_, err := s.pool.Exec(ctx, `
			UPDATE pi_session_logs
			   SET body_location = $2, object_key = $3,
			       content_gzip = CASE WHEN $4 THEN NULL ELSE content_gzip END
			 WHERE id = $1 AND body_location = 'postgres' AND content_sha256 = $5
		`, p.id, location, key, opts.DropPostgresBodies, sha)
		if err != nil {
			return 0, fmt.Errorf("record migrated Pi session log %s: %w", p.id, err)
		}
		return int64(len(content)), nil
	})
}

func normalizeMigrationOptions(opts BodyMigrationOptions) BodyMigrationOptions {
	if opts.Batch <= 0 {
		opts.Batch = 200
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 4
	}
	return opts
}

// migrateInBatches selects pending rows and moves them with bounded
// concurrency until none remain or MaxRows is reached. A row that fails stops
// the run: it would otherwise be selected again forever.
func migrateInBatches(
	ctx context.Context,
	opts BodyMigrationOptions,
	stats *BodyMigrationStats,
	progress func(BodyMigrationStats),
	next func(ctx context.Context, limit int) ([]pendingBody, error),
	move func(ctx context.Context, p pendingBody) (int64, error),
) error {
	for {
		limit := opts.Batch
		if opts.MaxRows > 0 {
			limit = min(limit, opts.MaxRows-stats.Moved)
			if limit <= 0 {
				return nil
			}
		}
		batch, err := next(ctx, limit)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}

		var mu sync.Mutex
		var firstErr error
		sem := make(chan struct{}, opts.Concurrency)
		var wg sync.WaitGroup
		for _, p := range batch {
			wg.Add(1)
			sem <- struct{}{}
			go func(p pendingBody) {
				defer wg.Done()
				defer func() { <-sem }()
				n, err := move(ctx, p)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					if firstErr == nil {
						firstErr = err
					}
					return
				}
				stats.Moved++
				stats.Bytes += n
			}(p)
		}
		wg.Wait()
		if progress != nil {
			progress(*stats)
		}
		if firstErr != nil {
			return firstErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}
