package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestPoolerCancellationAndBackendLoss(t *testing.T) {
	directDSN := os.Getenv("PIPROXY_TEST_POSTGRES_DIRECT_DSN")
	if directDSN == "" {
		t.Skip("disposable pooler database is not configured")
	}
	pool := openIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	short, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	_, err := pool.Exec(short, "SELECT pg_sleep(5)")
	stop()
	if err == nil {
		t.Fatal("query did not honor cancellation")
	}
	if err := pool.Health(ctx); err != nil {
		t.Fatalf("pool did not recover from cancellation: %v", err)
	}

	// Pin an owned scratch backend before terminating it. No other database or
	// unpinned shared connection can be selected by this failure injection.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var pid int
	if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	direct, err := pgx.Connect(ctx, directDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close(context.Background())
	var terminated bool
	if err := direct.QueryRow(ctx, "SELECT pg_terminate_backend($1)", pid).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("scratch backend termination failed: %v", err)
	}
	if _, err := tx.Exec(ctx, "SELECT 1"); err == nil {
		t.Fatal("terminated transaction unexpectedly succeeded")
	}
	_ = tx.Rollback(ctx)
	// PgBouncer may have another stale backend queued after query cancellation;
	// allow time to discard it, then require a fresh query to succeed.
	for i := 0; i < 20; i++ {
		if err := pool.Health(ctx); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatal("pool did not recover from scratch backend loss")
}
