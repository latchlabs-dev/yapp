package postgres

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/earlgrey/piproxy/internal/blob"
)

func piLogRecord(tenantID, sessionID, content string) *PiSessionLogRecord {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(content))
	_ = zw.Close()
	sum := sha256.Sum256([]byte(content))
	return &PiSessionLogRecord{
		TenantID: tenantID, OwnerID: "key:none", SessionID: sessionID,
		ContentSHA256: hex.EncodeToString(sum[:]), UncompressedBytes: int64(len(content)), EntryCount: 1,
		ContentGzip: buf.Bytes(),
	}
}

func TestPiSessionLogsInObjectStorage(t *testing.T) {
	pool := openIntegrationPool(t)
	ctx := context.Background()
	tenantID := seedArchiveTenant(t, pool)
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM pi_session_logs WHERE tenant_id = $1`, tenantID) })

	objects := blob.NewMemory()
	store := NewPiSessionLogStore(pool).WithObjects(objects, true, false)

	first := piLogRecord(tenantID, "sess-1", `{"type":"session","id":"sess-1"}`+"\n")
	if err := store.Put(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := piLogRecord(tenantID, "sess-1", `{"type":"session","id":"sess-1"}`+"\n"+`{"type":"message"}`+"\n")
	if err := store.Put(ctx, second); err != nil {
		t.Fatal(err)
	}

	keys := objects.Keys()
	if len(keys) != 1 || keys[0] == "" || !bytes.Contains([]byte(keys[0]), []byte(second.ContentSHA256)) {
		t.Fatalf("objects after re-upload = %v, want only the latest content", keys)
	}
	var contentNull bool
	var location string
	if err := pool.QueryRow(ctx, `SELECT content_gzip IS NULL, body_location FROM pi_session_logs WHERE tenant_id = $1`, tenantID).Scan(&contentNull, &location); err != nil {
		t.Fatal(err)
	}
	if !contentNull || location != "memory" {
		t.Fatalf("row content null=%v location=%q", contentNull, location)
	}

	rec, raw, err := store.GetBySession(ctx, tenantID, "key:none", "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.ContentSHA256 != second.ContentSHA256 || string(raw) != `{"type":"session","id":"sess-1"}`+"\n"+`{"type":"message"}`+"\n" {
		t.Fatalf("read back %q", raw)
	}

	deleted, err := store.DeleteOlderThan(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.GetBySession(ctx, tenantID, "key:none", "sess-1"); err != nil || deleted != 0 {
		t.Fatalf("retention removed an object-backed export: deleted=%d err=%v", deleted, err)
	}

	legacy := NewPiSessionLogStore(pool)
	if err := legacy.Put(ctx, piLogRecord(tenantID, "sess-2", "x\n")); err != nil {
		t.Fatal(err)
	}
	if _, raw, err := legacy.GetBySession(ctx, tenantID, "key:none", "sess-2"); err != nil || string(raw) != "x\n" {
		t.Fatalf("postgres-backed read: %q %v", raw, err)
	}
	if _, _, err := legacy.GetBySession(ctx, tenantID, "key:none", "sess-1"); err == nil {
		t.Fatal("reading an object-backed export without objects must fail")
	}
}
