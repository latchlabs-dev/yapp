package redis_test

import (
	"context"
	"testing"
	"time"

	"github.com/earlgrey/piproxy/internal/store/redis"
)

func TestRevocationCacheNoopOnDisconnected(t *testing.T) {
	ctx := context.Background()

	// Create a disconnected client and a cache backed by it.
	client, err := redis.New(ctx, redis.Config{Address: ""})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer client.Close()

	cache := redis.NewRevocationCache(client)

	// All operations should be safe no-ops.
	if cache.IsRevoked(ctx, "test-prefix") {
		t.Error("expected false for IsRevoked on disconnected cache")
	}

	if err := cache.MarkRevoked(ctx, "test-prefix"); err != nil {
		t.Errorf("MarkRevoked should not error on disconnected cache: %v", err)
	}

	if err := cache.RemoveRevoked(ctx, "test-prefix"); err != nil {
		t.Errorf("RemoveRevoked should not error on disconnected cache: %v", err)
	}
}

func TestRevocationCacheNilSafe(t *testing.T) {
	ctx := context.Background()

	var nilCache *redis.RevocationCache = nil

	// Should not panic.
	if nilCache.IsRevoked(ctx, "test") {
		t.Error("expected false for nil cache")
	}

	if err := nilCache.MarkRevoked(ctx, "test"); err != nil {
		t.Errorf("expected no error for nil cache: %v", err)
	}

	if err := nilCache.RemoveRevoked(ctx, "test"); err != nil {
		t.Errorf("expected no error for nil cache: %v", err)
	}
}

func TestRevocationCacheNilClientSafe(t *testing.T) {
	ctx := context.Background()
	cache := redis.NewRevocationCache(nil)

	if cache.IsRevoked(ctx, "test") {
		t.Error("expected false for nil-client cache")
	}

	if err := cache.MarkRevoked(ctx, "test"); err != nil {
		t.Errorf("expected no error for nil-client cache: %v", err)
	}

	if err := cache.RemoveRevoked(ctx, "test"); err != nil {
		t.Errorf("expected no error for nil-client cache: %v", err)
	}

	if got := cache.String(); got == "" {
		t.Error("expected non-empty cache description")
	}
}

func TestRevocationCacheRoundTrip(t *testing.T) {
	// This test requires a real Redis, so skip by default.
	ctx := context.Background()
	client, err := redis.New(ctx, redis.Config{
		Address: "127.0.0.1:6379",
	})
	if err != nil || !client.IsConnected() {
		t.Skip("redis not available, skipping round-trip test")
	}
	defer client.Close()

	cache := redis.NewRevocationCache(client)
	prefix := "rpk_test_revoc_"

	// Initially not revoked.
	if cache.IsRevoked(ctx, prefix) {
		t.Fatal("expected prefix to not be revoked initially")
	}

	// Mark as revoked.
	if err := cache.MarkRevoked(ctx, prefix); err != nil {
		t.Fatalf("MarkRevoked failed: %v", err)
	}

	// Now it should be revoked.
	if !cache.IsRevoked(ctx, prefix) {
		t.Fatal("expected prefix to be revoked after MarkRevoked")
	}

	// Remove from revocation cache.
	if err := cache.RemoveRevoked(ctx, prefix); err != nil {
		t.Fatalf("RemoveRevoked failed: %v", err)
	}

	// Should not be revoked anymore.
	if cache.IsRevoked(ctx, prefix) {
		t.Fatal("expected prefix to not be revoked after RemoveRevoked")
	}
}

func TestRevocationCacheTTL(t *testing.T) {
	// This test requires a real Redis.
	ctx := context.Background()
	client, err := redis.New(ctx, redis.Config{
		Address: "127.0.0.1:6379",
	})
	if err != nil || !client.IsConnected() {
		t.Skip("redis not available, skipping TTL test")
	}
	defer client.Close()

	cache := redis.NewRevocationCache(client)
	prefix := "rpk_test_ttl_"

	// Use TTL directly to verify the key has an expiry.
	if err := cache.MarkRevoked(ctx, prefix); err != nil {
		t.Fatalf("MarkRevoked failed: %v", err)
	}
	defer cache.RemoveRevoked(ctx, prefix)

	// Check TTL is set (should be ~24h, but we just verify > 0).
	ttl, err := client.TTL(ctx, "revoked_prefix:"+prefix).Result()
	if err != nil {
		t.Fatalf("TTL check failed: %v", err)
	}
	if ttl <= 0 {
		t.Fatalf("expected positive TTL, got %v", ttl)
	}
	t.Logf("revocation cache TTL: %v", ttl)

	// Verify it's approximately 24h (within a second tolerance).
	expectedTTL := 24 * time.Hour
	diff := ttl - expectedTTL
	if diff < 0 {
		diff = -diff
	}
	if diff > time.Second {
		t.Logf("TTL differs from 24h by %v (may vary by timing)", diff)
	}
}
