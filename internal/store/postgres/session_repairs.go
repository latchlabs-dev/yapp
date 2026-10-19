package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/earlgrey/piproxy/internal/id"
	"github.com/earlgrey/piproxy/internal/sessionarchive"
)

var ErrRepairLeaseLost = errors.New("transcript repair lease lost")
var ErrArchiveIntegrity = errors.New("archived capture differs from source")

const RepairLeaseTTL = 2 * time.Minute

type RepairLease struct {
	TranscriptID, TenantID, ConversationID, Token string
	ExpiresAt                                     time.Time
	Attempts                                      int
}

// EnqueueRepair is idempotent while pending, including when the UI polls.
// Never advance rebuild_requested_at on those polls: that would prevent a
// running rebuild from clearing the flag.
func (s *SessionArchiveStore) EnqueueRepair(ctx context.Context, tenant, conversation string) (bool, error) {
	if conversation == "" {
		return false, nil
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO session_transcripts
 (id,tenant_id,conversation_id,started_at,last_activity_at,needs_rebuild,rebuild_requested_at)
 SELECT $1,$2,$3,MIN(created_at),MAX(created_at),true,now() FROM conversation_payloads
 WHERE COALESCE(tenant_id,'')=$4 AND conversation_id=$3 HAVING COUNT(*)>0
 ON CONFLICT ((COALESCE(tenant_id,'')),conversation_id) DO UPDATE SET
 needs_rebuild=true,
 rebuild_requested_at=CASE WHEN session_transcripts.needs_rebuild THEN session_transcripts.rebuild_requested_at ELSE now() END,
 repair_retry_at=CASE WHEN session_transcripts.needs_rebuild THEN session_transcripts.repair_retry_at ELSE NULL END`,
		id.New("strn"), nullableText(tenant), conversation, tenant)
	return tag.RowsAffected() > 0, err
}

func (s *SessionArchiveStore) ClaimRepair(ctx context.Context) (*RepairLease, error) {
	lease := &RepairLease{Token: id.New("repair")}
	err := s.pool.QueryRow(ctx, `UPDATE session_transcripts SET repair_lease_token=$1,
 repair_lease_expires_at=clock_timestamp()+INTERVAL '2 minutes',repair_attempts=repair_attempts+1
 WHERE id=(SELECT id FROM session_transcripts WHERE needs_rebuild
 AND (repair_retry_at IS NULL OR repair_retry_at<=clock_timestamp())
 AND (repair_lease_expires_at IS NULL OR repair_lease_expires_at<=clock_timestamp())
 ORDER BY repair_retry_at NULLS FIRST,rebuild_requested_at NULLS FIRST,id
 FOR UPDATE SKIP LOCKED LIMIT 1)
 RETURNING id,COALESCE(tenant_id,''),conversation_id,repair_lease_expires_at,repair_attempts`, lease.Token).
		Scan(&lease.TranscriptID, &lease.TenantID, &lease.ConversationID, &lease.ExpiresAt, &lease.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return lease, err
}

func (s *SessionArchiveStore) RenewRepair(ctx context.Context, lease *RepairLease) (time.Time, error) {
	var expiry time.Time
	err := s.pool.QueryRow(ctx, `UPDATE session_transcripts SET repair_lease_expires_at=clock_timestamp()+INTERVAL '2 minutes'
 WHERE id=$1 AND repair_lease_token=$2 AND repair_lease_expires_at>clock_timestamp()
 RETURNING repair_lease_expires_at`, lease.TranscriptID, lease.Token).Scan(&expiry)
	if errors.Is(err, pgx.ErrNoRows) {
		return expiry, ErrRepairLeaseLost
	}
	return expiry, err
}

// FinishRepair cannot release another replica's claim. Keep failed jobs
// pending with persisted backoff so one bad body cannot starve the backlog.
func (s *SessionArchiveStore) FinishRepair(ctx context.Context, lease *RepairLease, cause string) error {
	delay := time.Duration(5<<min(max(lease.Attempts-1, 0), 6)) * time.Second
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	_, err := s.pool.Exec(ctx, `UPDATE session_transcripts SET
 repair_lease_token=NULL,repair_lease_expires_at=NULL,
 repair_retry_at=CASE WHEN NOT needs_rebuild THEN NULL ELSE clock_timestamp()+$3::interval END,
 repair_attempts=CASE WHEN NOT needs_rebuild THEN 0 ELSE repair_attempts END,
 last_error=CASE WHEN $4<>'' AND needs_rebuild THEN $4 WHEN NOT needs_rebuild THEN NULL ELSE last_error END
 WHERE id=$1 AND repair_lease_token=$2`, lease.TranscriptID, lease.Token, fmt.Sprintf("%f seconds", delay.Seconds()), cause)
	return err
}

func (s *SessionArchiveStore) RepairBacklog(ctx context.Context) (int64, time.Time, error) {
	var count int64
	var oldest *time.Time
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*),MIN(COALESCE(rebuild_requested_at,created_at)) FROM session_transcripts WHERE needs_rebuild`).Scan(&count, &oldest)
	if oldest == nil {
		return count, time.Time{}, err
	}
	return count, *oldest, err
}

// ArchiveConversation preserves inline bodies, verifies both durable copies,
// and only then switches the row to permanent object-backed retention. An xmin
// guard prevents a concurrent capture update from being overwritten.
func (s *ConversationPayloadStore) ArchiveConversation(ctx context.Context, tenant, conversation string) (int, error) {
	if s.objects == nil || !s.rawInObjects {
		return 0, nil
	}
	moved := 0
	for {
		var requestID, xmin string
		var created time.Time
		var req, resp []byte
		err := s.pool.QueryRow(ctx, `SELECT request_id,created_at,request_body_compressed,response_body_compressed,xmin::text
   FROM conversation_payloads WHERE COALESCE(tenant_id,'')=$1 AND conversation_id=$2
   AND body_location='postgres' ORDER BY created_at,request_id LIMIT 1`, tenant, conversation).
			Scan(&requestID, &created, &req, &resp, &xmin)
		if errors.Is(err, pgx.ErrNoRows) {
			return moved, nil
		}
		if err != nil {
			return moved, err
		}
		if req == nil || resp == nil {
			return moved, fmt.Errorf("missing capture body: %w", ErrArchiveIntegrity)
		}
		if len(req)+len(resp) > 128<<20 {
			return moved, fmt.Errorf("capture exceeds archival memory budget: %w", ErrArchiveIntegrity)
		}
		requestKey := sessionarchive.CaptureRequestKey(tenant, created, requestID)
		responseKey := sessionarchive.CaptureResponseKey(tenant, created, requestID)
		for _, body := range []struct {
			key  string
			data []byte
		}{{requestKey, req}, {responseKey, resp}} {
			put := s.objects.Put
			if immutable, ok := s.objects.(interface {
				PutImmutable(context.Context, string, []byte, string) error
			}); ok {
				put = immutable.PutImmutable
			}
			if err := put(ctx, body.key, body.data, gzipContentType); err != nil {
				return moved, err
			}
			durable, err := s.objects.Read(ctx, body.key)
			if err != nil {
				return moved, err
			}
			if !bytes.Equal(durable, body.data) {
				return moved, ErrArchiveIntegrity
			}
		}
		tag, err := s.pool.Exec(ctx, `UPDATE conversation_payloads SET body_location=$2,
   request_object_key=$3,response_object_key=$4,expires_at=NULL
   WHERE request_id=$1 AND body_location='postgres' AND xmin::text=$5`,
			requestID, s.objects.Location(), requestKey, responseKey, xmin)
		if err != nil {
			return moved, err
		}
		moved += int(tag.RowsAffected())
	}
}
