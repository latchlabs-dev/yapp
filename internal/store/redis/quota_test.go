package redis_test

import (
	"context"
	"testing"
	"time"

	"github.com/earlgrey/piproxy/internal/store/redis"
)

// TestConcurrencyIncrRefreshesTTL verifies that every ConcurrencyIncr sets/refreshes
// the safety TTL — not just the first increment that creates the key — and that the
// TTL is longer than the maximum legitimate slot hold so an active stream's slot is
// never reclaimed mid-flight.
func TestConcurrencyIncrRefreshesTTL(t *testing.T) {
	ctx := context.Background()
	client, err := redis.New(ctx, redis.Config{Address: "127.0.0.1:6379"})
	if err != nil || !client.IsConnected() {
		t.Skip("redis not available, skipping concurrency TTL test")
	}
	defer client.Close()

	key := "quota:concurrency:tenant:ten_ttl_test"
	_ = client.Del(ctx, key).Err()
	t.Cleanup(func() { _ = client.Del(ctx, key).Err() })

	// First increment creates the key and must set a TTL.
	if _, err := client.ConcurrencyIncr(ctx, key); err != nil {
		t.Fatalf("first ConcurrencyIncr failed: %v", err)
	}
	ttl1, err := client.TTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("TTL after first incr failed: %v", err)
	}
	// The safety TTL must comfortably exceed max_stream_duration (600s default)
	// so an in-flight stream's slot cannot be reclaimed while still active.
	if ttl1 <= 10*time.Minute {
		t.Fatalf("expected safety TTL > 10m (must exceed max_stream_duration), got %v", ttl1)
	}

	// A subsequent increment on the still-live key must REFRESH the TTL rather
	// than leave the original expiry to fire while the counter is still busy.
	if _, err := client.ConcurrencyIncr(ctx, key); err != nil {
		t.Fatalf("second ConcurrencyIncr failed: %v", err)
	}
	ttl2, err := client.TTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("TTL after second incr failed: %v", err)
	}
	if ttl2 <= 0 {
		t.Fatalf("expected positive TTL after second incr, got %v", ttl2)
	}
	// The refreshed TTL should be at least as long as the first (it slid forward,
	// not decayed toward the original absolute expiry).
	if ttl2+2*time.Second < ttl1 {
		t.Fatalf("expected refreshed TTL >= first TTL, got ttl1=%v ttl2=%v", ttl1, ttl2)
	}

	// Draining the counter to zero deletes the key (no lingering TTL/slot).
	if _, err := client.ConcurrencyDecr(ctx, key); err != nil {
		t.Fatalf("first ConcurrencyDecr failed: %v", err)
	}
	if _, err := client.ConcurrencyDecr(ctx, key); err != nil {
		t.Fatalf("second ConcurrencyDecr failed: %v", err)
	}
	exists, err := client.Exists(ctx, key).Result()
	if err != nil {
		t.Fatalf("Exists check failed: %v", err)
	}
	if exists != 0 {
		t.Fatalf("expected key deleted after draining to zero, exists=%d", exists)
	}
}

func TestApplySpendSettlementIsIdempotent(t *testing.T) {
	ctx := context.Background()
	client, err := redis.New(ctx, redis.Config{Address: "127.0.0.1:6379"})
	if err != nil || !client.IsConnected() {
		t.Skip("redis not available, skipping spend settlement test")
	}
	defer client.Close()

	key := "quota:daily:spend_millicents:api_key:key_settlement_test:2099-01-01"
	markerKey := key + ":settlements"
	_ = client.Del(ctx, key, markerKey).Err()
	t.Cleanup(func() { _ = client.Del(ctx, key, markerKey).Err() })

	first, err := client.ApplySpendSettlement(ctx, key, "settlement_1", 1250, time.Hour)
	if err != nil {
		t.Fatalf("first settlement apply: %v", err)
	}
	second, err := client.ApplySpendSettlement(ctx, key, "settlement_1", 1250, time.Hour)
	if err != nil {
		t.Fatalf("settlement retry: %v", err)
	}
	if first != 1250 || second != 1250 {
		t.Fatalf("idempotent totals = %d then %d, want 1250 then 1250", first, second)
	}

	third, err := client.ApplySpendSettlement(ctx, key, "settlement_2", 750, time.Hour)
	if err != nil {
		t.Fatalf("second distinct settlement: %v", err)
	}
	if third != 2000 {
		t.Fatalf("distinct settlement total = %d, want 2000", third)
	}
}

func TestEnsureSpendFloorNeverDoubleChargesExistingSpend(t *testing.T) {
	ctx := context.Background()
	client, err := redis.New(ctx, redis.Config{Address: "127.0.0.1:6379"})
	if err != nil || !client.IsConnected() {
		t.Skip("redis not available, skipping spend floor test")
	}
	defer client.Close()

	key := "quota:daily:spend_millicents:api_key:key_floor_test:2099-01-01"
	markerKey := key + ":settlements"
	_ = client.Del(ctx, key, markerKey).Err()
	t.Cleanup(func() { _ = client.Del(ctx, key, markerKey).Err() })
	if err := client.Set(ctx, key, 750, time.Hour).Err(); err != nil {
		t.Fatalf("seed spend counter: %v", err)
	}

	first, err := client.EnsureSpendFloor(ctx, key, "floor_1", 1000, time.Hour)
	if err != nil {
		t.Fatalf("first floor: %v", err)
	}
	second, err := client.EnsureSpendFloor(ctx, key, "floor_1", 1000, time.Hour)
	if err != nil {
		t.Fatalf("floor retry: %v", err)
	}
	third, err := client.EnsureSpendFloor(ctx, key, "floor_2", 900, time.Hour)
	if err != nil {
		t.Fatalf("lower floor: %v", err)
	}
	if first != 1000 || second != 1000 || third != 1000 {
		t.Fatalf("floor totals = %d, %d, %d; want 1000 throughout", first, second, third)
	}
}
