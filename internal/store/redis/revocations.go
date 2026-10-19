// Package redis provides Redis-backed quota enforcement, revocation cache, and connection management.
//
// RevocationCache provides fast key revocation checks via Redis, so the auth
// middleware can reject revoked keys without querying Postgres on every request.
// Degrades gracefully when Redis is unavailable (skips cache, falls through to
// Postgres, matching the existing behavior).
package redis

import (
	"context"
	"fmt"
	"time"
)

const (
	revocationKeyPrefix = "revoked_prefix:" // stores prefix -> unix timestamp
	revocationTTL       = 24 * time.Hour
)

// RevocationCache provides fast Redis-backed revocation checks.
// Keys are cached by their visible prefix for O(1) lookups.
type RevocationCache struct {
	client *Client
}

// NewRevocationCache creates a new RevocationCache.
// Returns a no-op cache if the client is nil or disconnected.
func NewRevocationCache(client *Client) *RevocationCache {
	return &RevocationCache{client: client}
}

// IsRevoked checks if a key prefix has been cached as revoked.
// Returns false (safe default) when Redis is unavailable — the
// caller falls through to the authoritative Postgres status check.
func (c *RevocationCache) IsRevoked(ctx context.Context, prefix string) bool {
	if c == nil || c.client == nil || !c.client.IsConnected() {
		return false
	}
	val, err := c.client.Get(ctx, revocationKeyPrefix+prefix).Result()
	if err != nil || val == "" {
		return false
	}
	return true
}

// MarkRevoked caches a key prefix as revoked with a TTL.
// No-op when Redis is unavailable.
func (c *RevocationCache) MarkRevoked(ctx context.Context, prefix string) error {
	if c == nil || c.client == nil || !c.client.IsConnected() {
		return nil
	}
	return c.client.Set(ctx, revocationKeyPrefix+prefix, time.Now().Unix(), revocationTTL).Err()
}

// RemoveRevoked removes a prefix from the revocation cache (for un-revocation).
// No-op when Redis is unavailable.
func (c *RevocationCache) RemoveRevoked(ctx context.Context, prefix string) error {
	if c == nil || c.client == nil || !c.client.IsConnected() {
		return nil
	}
	return c.client.Del(ctx, revocationKeyPrefix+prefix).Err()
}

// String returns a description of the revocation cache.
func (c *RevocationCache) String() string {
	if c == nil || c.client == nil || !c.client.IsConnected() {
		return "revocation-cache(disconnected)"
	}
	return fmt.Sprintf("revocation-cache(ttl=%s)", revocationTTL)
}
