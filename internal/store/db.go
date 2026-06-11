package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
)

// DB wraps a pgx connection pool with OCR-specific query helpers.
type DB struct {
	Pool *pgxpool.Pool
}

// New parses the DSN, sizes a conservative pool, opens it, and pings to verify
// connectivity. The returned DB must be closed with Close.
func New(ctx context.Context, dsn string) (*DB, error) {
	if dsn == "" {
		return nil, errors.New("store: empty postgres DSN")
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: parse DSN: %w", err)
	}
	// Eight concurrent stages plus the API handlers and SSE reads share this pool. 16
	// connections left the busy paths queueing under load; 32 gives headroom.
	cfg.MaxConns = 32
	cfg.MinConns = 2
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: open pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}

	log.Info().Msg("store: postgres connected")
	return &DB{Pool: pool}, nil
}

// Close releases the underlying pool. Safe to call on a nil-pool DB.
func (d *DB) Close() {
	if d != nil && d.Pool != nil {
		d.Pool.Close()
	}
}
