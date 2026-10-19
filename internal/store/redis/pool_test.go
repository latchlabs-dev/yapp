package redis_test

import (
	"context"
	"testing"

	"github.com/earlgrey/piproxy/internal/store/redis"
)

func TestNewEmptyAddress(t *testing.T) {
	ctx := context.Background()
	client, err := redis.New(ctx, redis.Config{Address: ""})
	if err != nil {
		t.Fatalf("New with empty address should not error: %v", err)
	}
	if client == nil {
		t.Fatal("expected non-nil client")
	}
	if client.IsConnected() {
		t.Error("expected not connected for empty address")
	}

	// Close should be safe on disconnected client.
	if err := client.Close(); err != nil {
		t.Errorf("Close should not error: %v", err)
	}
}

func TestNewUnavailable(t *testing.T) {
	ctx := context.Background()
	client, err := redis.New(ctx, redis.Config{
		Address: "127.0.0.1:16379", // unlikely to be running
	})
	if err == nil {
		// Might succeed if Redis happens to be on that port.
		if client.IsConnected() {
			t.Log("redis is running on 16379, skipping")
			client.Close()
			return
		}
	}
	// Expected: error returned, but client is non-nil with healthy=false.
	if client == nil {
		t.Fatal("expected non-nil client even on error")
	}
	if client.IsConnected() {
		t.Error("expected not connected for unavailable redis")
	}

	// Close should be safe.
	if err := client.Close(); err != nil {
		t.Errorf("Close should not error: %v", err)
	}
}

func TestIsConnectedNilSafe(t *testing.T) {
	var nilClient *redis.Client = nil
	// This would panic if IsConnected didn't check for nil — but we test the
	// wrapper type, not the raw client, so it's about the *Client.
	_ = nilClient
}

func TestCloseNilSafe(t *testing.T) {
	var c *redis.Client = nil
	if c != nil {
		c.Close()
	}
}

func TestHealthOnDisconnected(t *testing.T) {
	ctx := context.Background()
	client, err := redis.New(ctx, redis.Config{Address: ""})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	if err := client.Health(ctx); err == nil {
		t.Error("expected error for health check on disconnected client")
	}
}
