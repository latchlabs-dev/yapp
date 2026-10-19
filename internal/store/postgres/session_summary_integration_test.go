package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/id"
	"github.com/earlgrey/piproxy/internal/sessionarchive"
)

func TestSessionSummarySeparatesAvailabilityAndRequestOutcomes(t *testing.T) {
	pool := openIntegrationPool(t)
	ctx := context.Background()
	tenant := seedArchiveTenant(t, pool)
	user := id.New("usr")
	_, err := pool.Exec(ctx, "INSERT INTO users (id, tenant_id, email) VALUES ($1,$2,$3)", user, tenant, user+"@example.test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM pi_session_logs WHERE tenant_id=$1", tenant)
		_, _ = pool.Exec(ctx, "DELETE FROM session_submissions WHERE tenant_id=$1", tenant)
		_, _ = pool.Exec(ctx, "DELETE FROM session_transcripts WHERE tenant_id=$1", tenant)
		_, _ = pool.Exec(ctx, "DELETE FROM conversation_payloads WHERE tenant_id=$1", tenant)
		_, _ = pool.Exec(ctx, "DELETE FROM users WHERE tenant_id=$1", tenant)
	})
	objects := NewSessionObjectStore(pool)
	payloads := NewConversationPayloadStore(pool).WithObjects(objects, true, false)
	states := NewSessionArchiveStore(pool)
	builder := sessionarchive.NewBuilder(objects, states, payloads, sessionarchive.BuilderOptions{})
	store := sessionarchive.NewCaptureStore(payloads, builder, zerolog.Nop(), nil)
	conversation := id.New("conv")
	captures := chatCaptures(tenant, conversation, 3, time.Now().UTC().Add(-time.Hour))
	captures[0].Status, captures[0].ErrorClass = "error", "client_closed"
	for _, p := range captures {
		p.UserID = user
		if err := store.Put(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	list := func() *SessionSummary {
		t.Helper()
		total, rows, err := payloads.ListSessions(ctx, tenant, "", "", 100, 0)
		if err != nil || total != 1 || len(rows) != 1 {
			t.Fatalf("list: total=%d rows=%d err=%v", total, len(rows), err)
		}
		return rows[0]
	}
	s := list()
	if s.Status != "success" || s.SuccessCount != 2 || s.InterruptedCount != 1 || s.FailureCount != 0 {
		t.Fatalf("mixed outcomes: %+v", s)
	}
	if s.TranscriptState != "available" || s.UploadState != "not_uploaded" || s.LastPromptText == "" {
		t.Fatalf("object-backed preview and availability: %+v", s)
	}
	var inline bool
	_ = pool.QueryRow(ctx, "SELECT request_body_compressed IS NOT NULL FROM conversation_payloads WHERE request_id=$1", captures[2].RequestID).Scan(&inline)
	if inline {
		t.Fatal("test must exercise object-only bodies")
	}
	// Another owner's upload must never change this owner's availability or preview.
	otherOwner := "user:" + id.New("other")
	submissions := NewSessionSubmissionStore(pool)
	wrong := &SessionSubmissionRecord{TenantID: tenant, OwnerID: otherOwner, ConversationID: conversation, FirstPrompt: "Wrong owner preview"}
	if err := submissions.Put(ctx, wrong, nil); err != nil {
		t.Fatal(err)
	}
	if s = list(); s.UploadState != "not_uploaded" {
		t.Fatalf("cross-owner metadata leaked: %+v", s)
	}
	submission := &SessionSubmissionRecord{TenantID: tenant, UserID: user, OwnerID: "user:" + user, ConversationID: conversation, FirstPrompt: "Uploaded opening prompt"}
	if err := submissions.Put(ctx, submission, nil); err != nil {
		t.Fatal(err)
	}
	if s = list(); s.UploadState != "pending" || s.FailureCount != 0 {
		t.Fatalf("awaiting upload: %+v", s)
	}
	log := &PiSessionLogRecord{TenantID: tenant, UserID: user, OwnerID: "user:" + user, SessionID: conversation, ContentSHA256: "synthetic", ContentGzip: []byte{1}, UncompressedBytes: 1}
	if err := NewPiSessionLogStore(pool).Put(ctx, log); err != nil {
		t.Fatal(err)
	}
	if s = list(); s.UploadState != "uploaded" {
		t.Fatalf("uploaded: %+v", s)
	}
	// A real failure is counted, but a later success remains the latest request outcome.
	if _, err := pool.Exec(ctx, "UPDATE conversation_payloads SET status='error',error_class='upstream_error' WHERE request_id=$1", captures[1].RequestID); err != nil {
		t.Fatal(err)
	}
	if s = list(); s.Status != "success" || s.FailureCount != 1 || s.InterruptedCount != 1 {
		t.Fatalf("failure count: %+v", s)
	}
	// Keep stale generations out of previews and expose rebuild state separately.
	if _, err := pool.Exec(ctx, "UPDATE session_transcripts SET needs_rebuild=true, generation=generation+1 WHERE tenant_id=$1", tenant); err != nil {
		t.Fatal(err)
	}
	if s = list(); s.TranscriptState != "rebuilding" || s.LastPromptText != "Uploaded opening prompt" {
		t.Fatalf("stale preview: %+v", s)
	}
	// A different tenant reusing the same conversation ID cannot borrow the upload.
	secondTenant := seedArchiveTenant(t, pool)
	other := chatCaptures(secondTenant, conversation, 1, time.Now().UTC())
	if err := payloads.Put(ctx, other[0]); err != nil {
		t.Fatal(err)
	}
	_, others, err := payloads.ListSessions(ctx, secondTenant, "", "", 100, 0)
	if err != nil || len(others) != 1 {
		t.Fatalf("other tenant: %v %v", others, err)
	}
	if others[0].UploadState != "not_uploaded" || others[0].TranscriptState != "not_available" {
		t.Fatalf("cross-tenant state: %+v", others[0])
	}
}

// Raw session browsing must work without an archive builder and filter before pagination.
func TestCapturedSessionScopeBeforePagination(t *testing.T) {
	pool := openIntegrationPool(t)
	ctx := context.Background()
	tenant := seedArchiveTenant(t, pool)
	a, b := id.New("usr"), id.New("usr")
	for _, user := range []string{a, b} {
		if _, err := pool.Exec(ctx, "INSERT INTO users (id,tenant_id,email) VALUES ($1,$2,$3)", user, tenant, user+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM conversation_payloads WHERE tenant_id=$1", tenant)
		_, _ = pool.Exec(ctx, "DELETE FROM users WHERE tenant_id=$1", tenant)
	})
	store := NewConversationPayloadStore(pool)
	conv := id.New("conv")
	start := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)
	captures := chatCaptures(tenant, conv, 105, start)
	for _, p := range captures {
		p.UserID = a
		if err := store.Put(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	other := chatCaptures(tenant, conv, 1, start)[0]
	other.UserID = b
	if err := store.Put(ctx, other); err != nil {
		t.Fatal(err)
	}
	later := chatCaptures(tenant, conv, 1, start.Add(time.Hour))[0]
	later.UserID = a
	if err := store.Put(ctx, later); err != nil {
		t.Fatal(err)
	}
	scope := PayloadSessionScope{TenantID: tenant, ConversationID: conv, UserID: a, From: captures[0].CreatedAt, To: captures[104].CreatedAt}
	total, rows, err := store.List(ctx, "", "", "", "", "", 100, 0, true, scope)
	if err != nil || total != 105 || len(rows) != 100 {
		t.Fatalf("page 1: total=%d rows=%d err=%v", total, len(rows), err)
	}
	for _, row := range rows {
		if row.UserID != a || row.CreatedAt.After(scope.To) || row.LastPromptText == "" {
			t.Fatalf("wrong capture: %+v", row)
		}
	}
	total, rows, err = store.List(ctx, "", "", "", "", "", 100, 100, true, scope)
	if err != nil || total != 105 || len(rows) != 5 {
		t.Fatalf("page 2: total=%d rows=%d err=%v", total, len(rows), err)
	}
	// Missing conversation/owner means anonymous captures, never all conversations/owners.
	scope.ConversationID = ""
	scope.UserID = ""
	total, _, err = store.List(ctx, "", "", "", "", "", 100, 0, true, scope)
	if err != nil || total != 0 {
		t.Fatalf("empty scope broadened: %d %v", total, err)
	}
}
