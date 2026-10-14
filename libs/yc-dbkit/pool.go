package dbkit

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Config is the `database:` block of every service config.
type Config struct {
	URL      string `koanf:"url"`
	MaxConns int32  `koanf:"max_conns"`
	MinConns int32  `koanf:"min_conns"`

	// MigrateOnStart applies embedded migrations before the service
	// serves traffic. On in dev; production runs yc-migrate instead.
	MigrateOnStart bool `koanf:"migrate_on_start"`

	ConnectTimeout time.Duration `koanf:"connect_timeout"`
}

// DefaultConfig is the baseline services start from before loading config.
func DefaultConfig() Config {
	return Config{MaxConns: 10, MinConns: 1, MigrateOnStart: true, ConnectTimeout: 10 * time.Second}
}

// Connect opens a pgx pool with query tracing and verifies it with a ping.
func Connect(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("dbkit: database url is required")
	}
	pc, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("dbkit: parse url: %w", err)
	}
	if cfg.MaxConns > 0 {
		pc.MaxConns = cfg.MaxConns
	}
	if cfg.MinConns > 0 {
		pc.MinConns = cfg.MinConns
	}
	pc.ConnConfig.Tracer = queryTracer{}

	timeout := cfg.ConnectTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("dbkit: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("dbkit: ping: %w", err)
	}
	return pool, nil
}
