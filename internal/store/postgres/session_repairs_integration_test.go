package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/blob"
	"github.com/earlgrey/piproxy/internal/id"
	"github.com/earlgrey/piproxy/internal/sessionarchive"
)

func TestRepairLeaseIdempotentQueueStealingAndFencing(t *testing.T) {
	pool := openIntegrationPool(t)
	ctx := context.Background()
	tenant := seedArchiveTenant(t, pool)
	conversation := id.New("conv")
	payloads := NewConversationPayloadStore(pool)
	p := chatCaptures(tenant, conversation, 1, time.Now().Add(-time.Hour))[0]
	if err := payloads.Put(ctx, p); err != nil {
		t.Fatal(err)
	}
	states := NewSessionArchiveStore(pool)
	for range 2 {
		if queued, err := states.EnqueueRepair(ctx, tenant, conversation); err != nil || !queued {
			t.Fatalf("enqueue %v %v", queued, err)
		}
	}
	lease, err := states.ClaimRepair(ctx)
	if err != nil || lease == nil {
		t.Fatalf("claim %v %v", lease, err)
	}
	if other, err := states.ClaimRepair(ctx); err != nil || other != nil {
		t.Fatalf("live lease stolen: %v %v", other, err)
	}
	before, _ := states.Get(ctx, tenant, conversation)
	if _, err := states.EnqueueRepair(ctx, tenant, conversation); err != nil {
		t.Fatal(err)
	}
	after, _ := states.Get(ctx, tenant, conversation)
	if !before.RebuildRequestedAt.Equal(*after.RebuildRequestedAt) {
		t.Fatal("polling requeued an active repair")
	}
	if _, err := pool.Exec(ctx, `UPDATE session_transcripts SET repair_lease_expires_at=now()-INTERVAL '1 second' WHERE id=$1`, lease.TranscriptID); err != nil {
		t.Fatal(err)
	}
	winner, err := states.ClaimRepair(ctx)
	if err != nil || winner == nil || winner.Token == lease.Token {
		t.Fatalf("expired lease not recoverable: %v %v", winner, err)
	}
	err = states.WithRepairLease(lease.Token).WithConversationLock(ctx, tenant, conversation, func(sessionarchive.StateTx) error { return nil })
	if !errors.Is(err, ErrRepairLeaseLost) {
		t.Fatalf("stale publisher accepted: %v", err)
	}
	if _, err := states.RenewRepair(ctx, lease); !errors.Is(err, ErrRepairLeaseLost) {
		t.Fatalf("old owner renewed: %v", err)
	}
	if err := states.FinishRepair(ctx, lease, "cancelled"); err != nil {
		t.Fatal(err)
	}
	if _, err := states.RenewRepair(ctx, winner); err != nil {
		t.Fatalf("stale release changed winner: %v", err)
	}
	if err := states.FinishRepair(ctx, winner, "source_missing"); err != nil {
		t.Fatal(err)
	}
	if immediate, err := states.ClaimRepair(ctx); err != nil || immediate != nil {
		t.Fatalf("persisted backoff ignored: %v %v", immediate, err)
	}
}

type corruptArchiveObjects struct{ *blob.Memory }

func (o corruptArchiveObjects) Read(ctx context.Context, key string) ([]byte, error) {
	data, err := o.Memory.Read(ctx, key)
	if err == nil {
		return append(data, 'x'), nil
	}
	return data, err
}

func TestArchiveVerificationPreservesBodiesAndRetentionUntilDurable(t *testing.T) {
	pool := openIntegrationPool(t)
	ctx := context.Background()
	tenant := seedArchiveTenant(t, pool)
	conversation := id.New("conv")
	payloads := NewConversationPayloadStore(pool)
	p := chatCaptures(tenant, conversation, 1, time.Now().Add(-time.Hour))[0]
	expiry := time.Now().Add(-time.Minute)
	p.ExpiresAt = &expiry
	if err := payloads.Put(ctx, p); err != nil {
		t.Fatal(err)
	}
	protected, err := payloads.DeleteExpired(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := payloads.Get(ctx, p.RequestID); err != nil {
		t.Fatalf("unbuilt source deleted (total deletions=%d): %v", protected, err)
	}
	memory := blob.NewMemory()
	bad := payloads.WithObjects(corruptArchiveObjects{memory}, true, true)
	if _, err := bad.ArchiveConversation(ctx, tenant, conversation); !errors.Is(err, ErrArchiveIntegrity) {
		t.Fatalf("corrupt durable copy accepted: %v", err)
	}
	var location string
	var retained bool
	_ = pool.QueryRow(ctx, `SELECT body_location,request_body_compressed IS NOT NULL AND response_body_compressed IS NOT NULL FROM conversation_payloads WHERE request_id=$1`, p.RequestID).Scan(&location, &retained)
	if location != "postgres" || !retained {
		t.Fatal("failed verification changed source")
	}
	good := payloads.WithObjects(memory, true, true)
	if n, err := good.ArchiveConversation(ctx, tenant, conversation); err != nil || n != 1 {
		t.Fatalf("valid archival %d %v", n, err)
	}
	var permanent bool
	_ = pool.QueryRow(ctx, `SELECT body_location,expires_at IS NULL,request_body_compressed IS NOT NULL FROM conversation_payloads WHERE request_id=$1`, p.RequestID).Scan(&location, &permanent, &retained)
	if location != "memory" || !permanent || !retained {
		t.Fatal("archival did not retain verified original and permanent metadata")
	}
	states := NewSessionArchiveStore(pool)
	_, _ = states.EnqueueRepair(ctx, tenant, conversation)
	lease, err := states.ClaimRepair(ctx)
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	builder := sessionarchive.NewBuilder(memory, states.WithRepairLease(lease.Token), good, sessionarchive.BuilderOptions{Logger: zerolog.Nop()})
	result, err := builder.Rebuild(ctx, tenant, conversation)
	if err != nil || result.NeedsRebuild || result.LastSeq != 1 {
		t.Fatalf("repair %v %v", result, err)
	}
	if err := states.FinishRepair(ctx, lease, ""); err != nil {
		t.Fatal(err)
	}
}

func TestReconciliationFindsUnflaggedStoredCaptureGap(t *testing.T) {
	pool := openIntegrationPool(t)
	ctx := context.Background()
	tenant := seedArchiveTenant(t, pool)
	conversation := id.New("conv")
	payloads := NewConversationPayloadStore(pool)
	states := NewSessionArchiveStore(pool)
	objects := blob.NewMemory()
	captures := chatCaptures(tenant, conversation, 2, time.Now().Add(-time.Hour))
	store := sessionarchive.NewCaptureStore(payloads, sessionarchive.NewBuilder(objects, states, payloads, sessionarchive.BuilderOptions{}), zerolog.Nop(), nil)
	if err := store.Put(ctx, captures[0]); err != nil {
		t.Fatal(err)
	}
	// Model a crash between raw storage and Apply; no repair flag was written.
	if err := payloads.Put(ctx, captures[1]); err != nil {
		t.Fatal(err)
	}
	refs, err := states.ConversationsNeedingTranscripts(ctx, BackfillFilter{TenantID: &tenant, UnqueuedOnly: true})
	if err != nil || len(refs) != 1 || refs[0].ConversationID != conversation || refs[0].RequestCount != 2 {
		t.Fatalf("orphan capture remains invisible: %v %v", refs, err)
	}
	if _, err := states.EnqueueRepair(ctx, tenant, conversation); err != nil {
		t.Fatal(err)
	}
	refs, err = states.ConversationsNeedingTranscripts(ctx, BackfillFilter{TenantID: &tenant, UnqueuedOnly: true})
	if err != nil || len(refs) != 0 {
		t.Fatalf("queued repair rediscovered: %v %v", refs, err)
	}
}
