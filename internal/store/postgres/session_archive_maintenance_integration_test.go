package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/earlgrey/piproxy/internal/blob"
	"github.com/earlgrey/piproxy/internal/id"
	"github.com/earlgrey/piproxy/internal/sessionarchive"
)

func TestMigrateBodiesToObjectsAndBackfillListing(t *testing.T) {
	pool := openIntegrationPool(t)
	ctx := context.Background()
	tenantID := seedArchiveTenant(t, pool)
	conversationID := id.New("conv")

	legacy := NewConversationPayloadStore(pool)
	captures := chatCaptures(tenantID, conversationID, 3, time.Now().Add(-2*time.Hour))
	expires := time.Now().Add(24 * time.Hour)
	for _, p := range captures {
		p.ExpiresAt = &expires
		if err := legacy.Put(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	states := NewSessionArchiveStore(pool)
	tenant := tenantID
	refs, err := states.ConversationsNeedingTranscripts(ctx, BackfillFilter{TenantID: &tenant})
	if err != nil || len(refs) != 1 || refs[0].ConversationID != conversationID || refs[0].RequestCount != 3 {
		t.Fatalf("backfill candidates: %+v %v", refs, err)
	}

	objects := blob.NewMemory()
	// Scoped to this tenant: the test DB is shared, and an unscoped run would
	// move (and later drop) every other fixture's bodies into this in-memory
	// store.
	migrator := NewConversationPayloadStore(pool).WithObjects(objects, true, true)
	dry, err := migrator.MigrateBodiesToObjects(ctx, BodyMigrationOptions{TenantID: &tenant, DryRun: true}, nil)
	if err != nil || dry.Pending != 3 {
		t.Fatalf("dry run: %+v %v", dry, err)
	}
	stats, err := migrator.MigrateBodiesToObjects(ctx, BodyMigrationOptions{TenantID: &tenant, Batch: 2, Concurrency: 2}, nil)
	if err != nil || stats.Moved != 3 {
		t.Fatalf("copy: %+v %v", stats, err)
	}
	var postgresRows, bothRows int
	_ = pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE body_location = 'postgres'),
	                               count(*) FILTER (WHERE body_location = 'memory' AND request_body_compressed IS NOT NULL AND expires_at IS NULL)
	                          FROM conversation_payloads WHERE tenant_id = $1`, tenantID).Scan(&postgresRows, &bothRows)
	if postgresRows != 0 || bothRows != 3 {
		t.Fatalf("after copy: postgres=%d both=%d", postgresRows, bothRows)
	}

	dropped, err := migrator.MigrateBodiesToObjects(ctx, BodyMigrationOptions{TenantID: &tenant, DropPostgresBodies: true}, nil)
	if err != nil || dropped.Dropped != 3 {
		t.Fatalf("drop: %+v %v", dropped, err)
	}
	var stillInPostgres int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM conversation_payloads WHERE tenant_id = $1 AND request_body_compressed IS NOT NULL`, tenantID).Scan(&stillInPostgres)
	if stillInPostgres != 0 {
		t.Fatalf("%d rows still hold Postgres bodies", stillInPostgres)
	}
	reader := NewConversationPayloadStore(pool).WithObjects(objects, false, true)
	for _, p := range captures {
		got, err := reader.Get(ctx, p.RequestID)
		if err != nil || string(got.RequestBody) != string(p.RequestBody) {
			t.Fatalf("migrated capture %s unreadable: %v", p.RequestID, err)
		}
	}
	if n, err := reader.ConversationBodyBytes(ctx, tenantID, conversationID); err != nil || n != 0 {
		t.Fatalf("postgres body bytes after drop = %d, %v", n, err)
	}

	builder := sessionarchive.NewBuilder(blob.NewMemory(), states, reader, sessionarchive.BuilderOptions{})
	if _, err := builder.Rebuild(ctx, tenantID, conversationID); err != nil {
		t.Fatalf("rebuild from migrated bodies: %v", err)
	}
	refs, _ = states.ConversationsNeedingTranscripts(ctx, BackfillFilter{TenantID: &tenant})
	if len(refs) != 0 {
		t.Fatalf("current transcript still listed for backfill: %+v", refs)
	}
	refs, _ = states.ConversationsNeedingTranscripts(ctx, BackfillFilter{TenantID: &tenant, All: true})
	if len(refs) != 1 {
		t.Fatalf("--all must include current transcripts: %+v", refs)
	}
}

func TestMigratePiLogsToObjects(t *testing.T) {
	pool := openIntegrationPool(t)
	ctx := context.Background()
	tenantID := seedArchiveTenant(t, pool)
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM pi_session_logs WHERE tenant_id = $1`, tenantID) })

	if err := NewPiSessionLogStore(pool).Put(ctx, piLogRecord(tenantID, "sess-m", "line\n")); err != nil {
		t.Fatal(err)
	}
	objects := blob.NewMemory()
	store := NewPiSessionLogStore(pool).WithObjects(objects, true, false)
	stats, err := store.MigrateToObjects(ctx, BodyMigrationOptions{TenantID: &tenantID, DropPostgresBodies: true}, nil)
	if err != nil || stats.Moved != 1 {
		t.Fatalf("migrate: %+v %v", stats, err)
	}
	var location string
	var contentNull bool
	_ = pool.QueryRow(ctx, `SELECT body_location, content_gzip IS NULL FROM pi_session_logs WHERE tenant_id = $1`, tenantID).Scan(&location, &contentNull)
	if location != "memory" || !contentNull {
		t.Fatalf("row after migration: %q null=%v", location, contentNull)
	}
	if _, raw, err := store.GetBySession(ctx, tenantID, "key:none", "sess-m"); err != nil || string(raw) != "line\n" {
		t.Fatalf("read migrated log: %q %v", raw, err)
	}
}
