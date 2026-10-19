package postgres

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/earlgrey/piproxy/internal/blob"
	"github.com/earlgrey/piproxy/internal/id"
)

func openIntegrationPool(t *testing.T) *Pool {
	t.Helper()
	dsn := os.Getenv("PIPROXY_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PIPROXY_TEST_POSTGRES_DSN is not set")
	}
	pool, err := New(context.Background(), Config{DSN: dsn, MaxOpenConns: 8, MaxIdleConns: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestSessionObjectStoreRoundTrip(t *testing.T) {
	pool := openIntegrationPool(t)
	ctx := context.Background()
	var objects blob.Objects = NewSessionObjectStore(pool)

	key := "test/" + id.New("obj")
	t.Cleanup(func() { _ = objects.Delete(ctx, key) })

	if err := objects.Put(ctx, key, []byte("first"), "application/gzip"); err != nil {
		t.Fatal(err)
	}
	if err := objects.Put(ctx, key, []byte("second"), "application/gzip"); err != nil {
		t.Fatalf("retried put must be idempotent: %v", err)
	}
	got, err := objects.Read(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second" {
		t.Fatalf("read %q, want second", got)
	}
	var size int64
	if err := pool.QueryRow(ctx, `SELECT size_bytes FROM session_objects WHERE key=$1`, key).Scan(&size); err != nil || size != 6 {
		t.Fatalf("size_bytes = %d, err %v", size, err)
	}
	if err := objects.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := objects.Delete(ctx, key); err != nil {
		t.Fatalf("delete absent: %v", err)
	}
	if _, err := objects.Read(ctx, key); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("read after delete err = %v, want blob.ErrNotFound", err)
	}
}
