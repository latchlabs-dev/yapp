package postgres

import (
	"context"
	"os"
	"testing"
)

func TestMemoryProjectResolverConvergesUsersAndSeparatesRepositories(t *testing.T) {
	dsn := os.Getenv("PIPROXY_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PIPROXY_TEST_POSTGRES_DSN is not set")
	}
	ctx := context.Background()
	pool, err := New(ctx, Config{DSN: dsn, MaxOpenConns: 4, MaxIdleConns: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	tenantID := "tn_memory_project_integration"
	if _, err := pool.Exec(ctx, `
		INSERT INTO tenants (id, name, status, created_at, updated_at)
		VALUES ($1, 'Memory project integration', 'active', now(), now())
		ON CONFLICT (id) DO NOTHING
	`, tenantID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM memory_project_remotes WHERE tenant_id=$1`, tenantID)
		_, _ = pool.Exec(ctx, `DELETE FROM memory_projects WHERE tenant_id=$1`, tenantID)
		_, _ = pool.Exec(ctx, `DELETE FROM tenants WHERE id=$1`, tenantID)
	})

	store := NewMemoryProjectStore(pool)
	first, err := store.Resolve(ctx, tenantID, "alice-checkout", []string{
		"https://github.com/example-org/example-app.git",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Resolve(ctx, tenantID, "bob-checkout", []string{
		"git@github.com:example-org/example-app.git",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("same repository resolved to %q and %q", first.ID, second.ID)
	}

	fork, err := store.Resolve(ctx, tenantID, "fork-checkout", []string{
		"https://github.com/example-org/example-app.git",
		"git@github.com:alice/example-app.git",
	})
	if err != nil {
		t.Fatal(err)
	}
	if fork.ID != first.ID {
		t.Fatalf("upstream alias resolved to %q, want %q", fork.ID, first.ID)
	}

	unrelated, err := store.Resolve(ctx, tenantID, "alice-checkout", []string{
		"git@gitlab.com:example-org/alice-checkout.git",
	})
	if err != nil {
		t.Fatal(err)
	}
	if unrelated.ID == first.ID {
		t.Fatal("different repositories with the same label were merged")
	}
}
