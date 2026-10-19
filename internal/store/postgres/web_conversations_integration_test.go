package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/earlgrey/piproxy/internal/id"
)

func webConversationFixture(t *testing.T) (*Pool, *WebConversationStore, *WebConversation) {
	t.Helper()
	pool := openIntegrationPool(t)
	ctx := context.Background()
	tenantID, userID := id.New("tn_webtest"), id.New("usr_webtest")
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ($1, 'Web turn test')`, tenantID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, tenant_id, email) VALUES ($1, $2, $3)`, userID, tenantID, userID+"@example.test"); err != nil {
		t.Fatal(err)
	}
	store := NewWebConversationStore(pool)
	conv, err := store.Create(ctx, &WebConversation{TenantID: tenantID, UserID: userID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM web_conversations WHERE tenant_id = $1`, tenantID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
		_, _ = pool.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, tenantID)
	})
	return pool, store, conv
}

func otherWebConversationStore(t *testing.T) (*Pool, *WebConversationStore) {
	t.Helper()
	pool, err := New(context.Background(), Config{DSN: os.Getenv("PIPROXY_TEST_POSTGRES_DSN"), MaxOpenConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, NewWebConversationStore(pool)
}

func turnMessage(conv *WebConversation, role string) *WebMessage {
	return &WebMessage{ConversationID: conv.ID, Role: role, Content: json.RawMessage(`[{"type":"text","text":"hello"}]`)}
}

func TestWebConversationConcurrentSequenceAllocation(t *testing.T) {
	_, first, conv := webConversationFixture(t)
	_, second := otherWebConversationStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	const workers = 40
	start := make(chan struct{})
	seqs := make(chan int, workers)
	errCh := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(store *WebConversationStore) {
			defer wg.Done()
			<-start
			message, err := store.AppendMessage(ctx, turnMessage(conv, "user"))
			if err != nil {
				errCh <- err
				return
			}
			seqs <- message.Seq
		}([]*WebConversationStore{first, second}[i%2])
	}
	close(start)
	wg.Wait()
	close(errCh)
	close(seqs)
	for err := range errCh {
		t.Errorf("concurrent append: %v", err)
	}
	seen := make(map[int]bool)
	for seq := range seqs {
		if seen[seq] {
			t.Errorf("duplicate sequence %d", seq)
		}
		seen[seq] = true
	}
	for seq := 0; seq < workers; seq++ {
		if !seen[seq] {
			t.Errorf("sequence %d missing; %d successful writes", seq, len(seen))
		}
	}
	// A failed insert must roll back its counter allocation too.
	bad := turnMessage(conv, "user")
	bad.Content = json.RawMessage(`{"invalid_json"`)
	if _, err := first.AppendMessage(ctx, bad); err == nil {
		t.Fatal("invalid JSON unexpectedly persisted")
	}
	message, err := second.AppendMessage(ctx, turnMessage(conv, "assistant"))
	if err != nil || message.Seq != workers {
		t.Fatalf("sequence after failed append = %v, %v", message, err)
	}
}

func TestWebConversationDistributedTurnLeaseAndHistory(t *testing.T) {
	firstPool, first, conv := webConversationFixture(t)
	secondPool, second := otherWebConversationStore(t)
	ctx := context.Background()
	lease, err := first.AcquireTurn(ctx, conv.TenantID, conv.UserID, conv.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.AppendMessageForTurn(ctx, turnMessage(conv, "user"), lease); err != nil {
		t.Fatal(err)
	}
	if _, err := second.AcquireTurn(ctx, conv.TenantID, conv.UserID, conv.ID, time.Minute); !errors.Is(err, ErrConversationBusy) {
		t.Fatalf("second replica entered active turn: %v", err)
	}
	if _, err := second.AppendMessage(ctx, turnMessage(conv, "user")); !errors.Is(err, ErrConversationBusy) {
		t.Fatalf("unleased append entered active turn: %v", err)
	}
	if firstPool.Stat().AcquiredConns() != 0 || secondPool.Stat().AcquiredConns() != 0 {
		t.Fatal("turn reservation retained a database connection")
	}
	if _, err := first.AppendMessageForTurn(ctx, turnMessage(conv, "assistant"), lease); err != nil {
		t.Fatal(err)
	}
	if err := first.ReleaseTurn(ctx, lease); err != nil {
		t.Fatal(err)
	}
	next, err := second.AcquireTurn(ctx, conv.TenantID, conv.UserID, conv.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer second.ReleaseTurn(ctx, next)
	if _, err := second.AppendMessageForTurn(ctx, turnMessage(conv, "user"), next); err != nil {
		t.Fatal(err)
	}
	history, err := second.ListMessages(ctx, conv.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 || history[0].Role != "user" || history[1].Role != "assistant" || history[2].Role != "user" {
		t.Fatalf("turn history interleaved: %+v", history)
	}
}

func TestWebConversationExpiredLeaseFencesOldWorker(t *testing.T) {
	pool, first, conv := webConversationFixture(t)
	_, second := otherWebConversationStore(t)
	ctx := context.Background()
	old, err := first.AcquireTurn(ctx, conv.TenantID, conv.UserID, conv.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE web_conversations SET turn_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, conv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := first.RenewTurn(ctx, old, time.Minute); !errors.Is(err, ErrTurnLeaseLost) {
		t.Fatalf("expired lease resurrected: %v", err)
	}
	if _, err := first.AppendMessageForTurn(ctx, turnMessage(conv, "assistant"), old); !errors.Is(err, ErrTurnLeaseLost) {
		t.Fatalf("expired worker appended a reply: %v", err)
	}
	current, err := second.AcquireTurn(ctx, conv.TenantID, conv.UserID, conv.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if current.Token == old.Token {
		t.Fatal("replacement lease reused the fencing token")
	}
	if err := first.ReleaseTurn(ctx, old); err != nil {
		t.Fatal(err)
	}
	if _, err := first.RenewTurn(ctx, old, time.Minute); !errors.Is(err, ErrTurnLeaseLost) {
		t.Fatalf("old token renewed the replacement lease: %v", err)
	}
	if _, err := first.AppendMessageForTurn(ctx, turnMessage(conv, "assistant"), old); !errors.Is(err, ErrTurnLeaseLost) {
		t.Fatalf("old token persisted after takeover: %v", err)
	}
	if _, err := second.AppendMessageForTurn(ctx, turnMessage(conv, "user"), current); err != nil {
		t.Fatalf("stale cleanup released replacement lease: %v", err)
	}
	if expires, err := second.RenewTurn(ctx, current, 2*time.Minute); err != nil || !expires.After(current.ExpiresAt) {
		t.Fatalf("current lease did not renew: %s, %v", expires, err)
	}
}

func TestWebConversationConcurrentLeaseAdmission(t *testing.T) {
	_, first, conv := webConversationFixture(t)
	_, second := otherWebConversationStore(t)
	ctx := context.Background()
	const attempts = 20
	start := make(chan struct{})
	leases := make(chan *WebTurnLease, attempts)
	errs := make(chan error, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(store *WebConversationStore) {
			defer wg.Done()
			<-start
			lease, err := store.AcquireTurn(ctx, conv.TenantID, conv.UserID, conv.ID, time.Minute)
			if err != nil {
				errs <- err
			} else {
				leases <- lease
			}
		}([]*WebConversationStore{first, second}[i%2])
	}
	close(start)
	wg.Wait()
	close(errs)
	if len(leases) != 1 {
		t.Fatalf("admitted %d concurrent turns, want one", len(leases))
	}
	for err := range errs {
		if !errors.Is(err, ErrConversationBusy) {
			t.Errorf("unexpected admission error: %v", err)
		}
	}
}

func TestWebConversationLeaseOwnershipAndArchive(t *testing.T) {
	_, store, conv := webConversationFixture(t)
	ctx := context.Background()
	for _, tc := range []struct{ tenant, user string }{{"other-tenant", conv.UserID}, {conv.TenantID, "other-user"}} {
		if _, err := store.AcquireTurn(ctx, tc.tenant, tc.user, conv.ID, time.Minute); !errors.Is(err, ErrConversationNotFound) {
			t.Fatalf("wrong principal acquired a turn: %v", err)
		}
	}
	lease, err := store.AcquireTurn(ctx, conv.TenantID, conv.UserID, conv.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Archive(ctx, conv.TenantID, conv.UserID, conv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RenewTurn(ctx, lease, time.Minute); !errors.Is(err, ErrTurnLeaseLost) {
		t.Fatalf("archived conversation renewed a turn: %v", err)
	}
	if _, err := store.AppendMessageForTurn(ctx, turnMessage(conv, "assistant"), lease); !errors.Is(err, ErrTurnLeaseLost) {
		t.Fatalf("archived conversation accepted a reply: %v", err)
	}
	if _, err := store.AcquireTurn(ctx, conv.TenantID, conv.UserID, conv.ID, time.Minute); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("archived conversation admitted a turn: %v", err)
	}
}

func TestWebConversationSequenceSurvivesLegacyWriterRollback(t *testing.T) {
	pool, store, conv := webConversationFixture(t)
	ctx := context.Background()
	// Simulate old binaries writing after the migration and during a later
	// application rollback. The compatibility trigger keeps the allocator
	// current, so the next new binary never claims a historical sequence.
	for round := 0; round < 3; round++ {
		for legacy := 0; legacy < 2; legacy++ {
			if _, err := pool.Exec(ctx,
				`INSERT INTO web_messages (id, conversation_id, seq, role)
				 SELECT $1, $2, COALESCE((SELECT MAX(seq)+1 FROM web_messages WHERE conversation_id=$2), 0), 'user'`,
				id.New("wmsg"), conv.ID); err != nil {
				t.Fatal(err)
			}
		}
		got, err := store.AppendMessage(ctx, turnMessage(conv, "assistant"))
		if err != nil || got.Seq != round*3+2 {
			t.Fatalf("new writer after legacy round %d: %+v, %v", round, got, err)
		}
	}
}
