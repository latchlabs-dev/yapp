// Package redis provides a Redis client and connection management.
// Designed to fail fast and silently when Redis is unavailable, so the proxy
// degrades gracefully without delaying startup.
package redis

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/redis/go-redis/v9"
)

// Client wraps a redis.Client with configuration.
type Client struct {
	*redis.Client
	cfg     Config
	healthy bool
}

// Config holds Redis connection configuration.
type Config struct {
	Address  string
	Password string
	DB       int
	TLS      bool
}

// New creates a new Redis client.
//
// Instead of relying on go-redis's built-in Ping (which triggers noisy
// internal retry logging on stderr), we perform our own lightweight TCP
// dial first. Only on success do we create the full go-redis client.
//
// Returns a noop client (healthy=false) for empty address or unreachable
// Redis, so callers never need to nil-check.
func New(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Address == "" {
		return &Client{healthy: false}, nil
	}

	// Fast TCP preflight — 1s timeout, no retries, no logging.
	dialer := &net.Dialer{Timeout: 1 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", cfg.Address)
	if err != nil {
		return &Client{healthy: false},
			fmt.Errorf("redis %s: %w", cfg.Address, err)
	}
	conn.Close()

	// Redis is reachable — create the full client.
	opts := &redis.Options{
		Addr:         cfg.Address,
		Password:     cfg.Password,
		DB:           cfg.DB,
		DialTimeout:  2 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
		PoolSize:     5,
		MinIdleConns: 0,
	}

	if cfg.TLS {
		opts.TLSConfig = nil
	}

	rdb := redis.NewClient(opts)

	// Verify with a proper Ping.
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		rdb.Close()
		return &Client{healthy: false},
			fmt.Errorf("redis %s: ping: %w", cfg.Address, err)
	}

	return &Client{Client: rdb, cfg: cfg, healthy: true}, nil
}

// IsConnected reports whether the client has a healthy Redis connection.
func (c *Client) IsConnected() bool {
	return c != nil && c.healthy && c.Client != nil
}

// Health checks Redis connectivity.
func (c *Client) Health(ctx context.Context) error {
	if !c.IsConnected() {
		return fmt.Errorf("redis not connected")
	}
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return c.Client.Ping(pingCtx).Err()
}

// Close closes the Redis client. Safe on nil or disconnected clients.
func (c *Client) Close() error {
	if c == nil || c.Client == nil {
		return nil
	}
	return c.Client.Close()
}
