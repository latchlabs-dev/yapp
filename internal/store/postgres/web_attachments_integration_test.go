package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/earlgrey/piproxy/internal/id"
)

func attachmentTestOwner(t *testing.T, pool *Pool) (string, string) {
	t.Helper()
	ctx := context.Background()
	tenant, user := id.New("tn"), id.New("usr")
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, name, status) VALUES ($1,'Attachment admission test','active')`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, tenant_id, email, status) VALUES ($1,$2,$3,'active')`, user, tenant, user+"@example.invalid"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM web_attachments WHERE tenant_id=$1`, tenant)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE tenant_id=$1`, tenant)
		_, _ = pool.Exec(ctx, `DELETE FROM tenants WHERE id=$1`, tenant)
	})
	return tenant, user
}

func reservation(tenant, user string, size int64) *WebAttachment {
	a := &WebAttachment{ID: id.New("watt"), TenantID: tenant, UserID: user, Filename: "test.txt", MimeType: "text/plain", Kind: "text", SizeBytes: size, Bucket: "test", UploadExpiresAt: time.Now().Add(10 * time.Minute)}
	a.ObjectKey = tenant + "/" + a.ID
	return a
}

func TestWebAttachmentConcurrentAdmission(t *testing.T) {
	pool := openIntegrationPool(t)
	tenant, user := attachmentTestOwner(t, pool)
	store := NewWebAttachmentStore(pool)
	limits := AttachmentLimits{UserBytes: 120, TenantBytes: 120, PendingPerUser: 50}
	start := make(chan struct{})
	results := make(chan error, 12)
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := store.CreatePending(context.Background(), reservation(tenant, user, 50), limits)
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrAttachmentBudgetExceeded) {
			t.Fatal(err)
		}
	}
	if success != 2 {
		t.Fatalf("concurrent reservations exceeded remaining storage: %d accepted", success)
	}
	otherUser := id.New("usr")
	if _, err := pool.Exec(context.Background(), `INSERT INTO users (id,tenant_id,email,status) VALUES ($1,$2,$3,'active')`, otherUser, tenant, otherUser+"@example.invalid"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreatePending(context.Background(), reservation(tenant, otherUser, 50), limits); !errors.Is(err, ErrAttachmentBudgetExceeded) {
		t.Fatalf("tenant total not enforced across users: %v", err)
	}
}

func TestWebAttachmentPendingLimitAndRejectedExpiry(t *testing.T) {
	pool := openIntegrationPool(t)
	tenant, user := attachmentTestOwner(t, pool)
	store := NewWebAttachmentStore(pool)
	limits := AttachmentLimits{UserBytes: 1000, TenantBytes: 1000, PendingPerUser: 1}
	a, err := store.CreatePending(context.Background(), reservation(tenant, user, 10), limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Reject(context.Background(), tenant, user, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreatePending(context.Background(), reservation(tenant, user, 10), limits); !errors.Is(err, ErrAttachmentBudgetExceeded) {
		t.Fatalf("rejection freed a live upload reservation: %v", err)
	}
	stale, err := store.ListSweepable(context.Background(), time.Hour, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range stale {
		if row.ID == a.ID {
			t.Fatal("cleanup would allow upload replay to recreate an orphan")
		}
	}
	if _, err := pool.Exec(context.Background(), `UPDATE web_attachments SET upload_expires_at=now()-INTERVAL '1 minute' WHERE id=$1`, a.ID); err != nil {
		t.Fatal(err)
	}
	stale, err = store.ListSweepable(context.Background(), time.Hour, 1000)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range stale {
		if row.ID == a.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("rejected object not sweepable after upload authority expiry")
	}
	if err := store.Delete(context.Background(), []string{a.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreatePending(context.Background(), reservation(tenant, user, 10), limits); err != nil {
		t.Fatalf("cleanup did not release capacity: %v", err)
	}
}

func TestWebAttachmentReadyBytesRemainReservedUntilDeletion(t *testing.T) {
	pool := openIntegrationPool(t)
	tenant, user := attachmentTestOwner(t, pool)
	store := NewWebAttachmentStore(pool)
	limits := AttachmentLimits{UserBytes: 100, TenantBytes: 1000, PendingPerUser: 16}
	a, err := store.CreatePending(context.Background(), reservation(tenant, user, 80), limits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Finalize(context.Background(), tenant, user, a.ID, "text/plain", "text", "digest", 80); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE web_attachments SET expires_at=now()-INTERVAL '1 minute' WHERE id=$1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreatePending(context.Background(), reservation(tenant, user, 30), limits); !errors.Is(err, ErrAttachmentBudgetExceeded) {
		t.Fatalf("stored bytes bypassed quota before cleanup: %v", err)
	}
}
