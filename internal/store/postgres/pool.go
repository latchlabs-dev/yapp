// Package postgres provides database access for the proxy.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool wraps a pgxpool.Pool with configuration.
type Pool struct {
	*pgxpool.Pool
	cfg          Config
	archiveSlots chan struct{}
}

// Config holds Postgres pool configuration.
type Config struct {
	DSN          string
	MaxOpenConns int
	MaxIdleConns int
}

// New creates a new Postgres connection pool.
func New(ctx context.Context, cfg Config) (*Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse postgres config: %w", err)
	}

	poolCfg.MaxConns = int32(cfg.MaxOpenConns)
	poolCfg.MinConns = int32(cfg.MaxIdleConns)
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.MaxConnIdleTime = 5 * time.Minute
	poolCfg.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create postgres pool: %w", err)
	}

	// Verify connectivity.
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	return &Pool{Pool: pool, cfg: cfg, archiveSlots: make(chan struct{}, 2)}, nil
}

// Health checks if the pool is reachable.
func (p *Pool) Health(ctx context.Context) error {
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return p.Pool.Ping(pingCtx)
}

// Close closes the pool.
func (p *Pool) Close() {
	p.Pool.Close()
}

// Stats returns pool statistics.
func (p *Pool) Stats() *pgxpool.Stat {
	return p.Pool.Stat()
}
