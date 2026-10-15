package eventbus

import (
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Config configures a Client.
type Config struct {
	// NATSURL is the connection string, e.g. nats://localhost:4222.
	// Required.
	NATSURL string

	// NATSOptions are passed to nats.Connect. Optional.
	NATSOptions []nats.Option

	// ServiceName is used as the source_service on every emitted event
	// and as a prefix in logs/traces. Required.
	ServiceName string

	// DB is the pgx pool for the service's own database. Required for
	// Publish and Subscribe; the library reads/writes outbox and
	// processed_events here. Leave nil only for publish-only tools that
	// use PublishDirect (yc-tenant-cli); no relay or janitor runs then.
	DB *pgxpool.Pool

	// DisableRelay stops this client forwarding outbox rows to NATS. Rows
	// stay pending until a client with the relay enabled starts. For
	// tests and the outbox crash drill; never set it in production.
	DisableRelay bool

	// Validator is called before publish and before handler dispatch.
	// Required; use NoopValidator in tests if genuinely needed.
	Validator Validator

	// Streams declares the JetStream streams this client requires. The
	// client ensures each stream exists at startup. If empty, the client
	// does not create streams (useful when another process manages them).
	Streams []StreamConfig

	// OutboxPollInterval is how often the relay checks for unpublished
	// events. Default: 100ms.
	OutboxPollInterval time.Duration

	// OutboxBatchSize is the max number of outbox rows per poll. Default: 50.
	OutboxBatchSize int

	// OutboxMaxAttempts before a row goes to the service's local DLQ.
	// Default: 5.
	OutboxMaxAttempts int

	// OutboxBackoff returns the wait before the next publish attempt.
	// Default: exponential starting at 1s, capped at 10m.
	OutboxBackoff BackoffFunc

	// ProcessedEventsRetention determines how long dedup rows are kept.
	// Default: 30 days.
	ProcessedEventsRetention time.Duration

	// OutboxCleanupRetention determines how long published outbox rows
	// are kept before deletion. Default: 7 days.
	OutboxCleanupRetention time.Duration

	// CleanupInterval governs the janitor goroutine. Default: 1 hour.
	CleanupInterval time.Duration
}

// StreamConfig is a minimal projection of jetstream.StreamConfig used by
// the client to ensure its streams exist. Full-power stream management
// (replicas, placement, encryption) belongs in ops tooling.
type StreamConfig struct {
	// Name is the JetStream stream name, e.g. "YC_ORDER".
	Name string

	// Subjects is the set of subject patterns the stream captures,
	// e.g. ["yc.*.order.>"].
	Subjects []string

	// Retention: file (default) or memory.
	Storage jetstream.StorageType

	// MaxAge is the age-based retention for the stream.
	MaxAge time.Duration

	// MaxBytes is the size-based retention. 0 means unlimited.
	MaxBytes int64
}

// BackoffFunc computes the wait before attempt n (0-indexed).
type BackoffFunc func(attempt int) time.Duration

// DefaultBackoff is 1s, 5s, 30s, 2m, 10m with jitter avoided for
// simplicity in v1.
func DefaultBackoff(attempt int) time.Duration {
	switch attempt {
	case 0:
		return 1 * time.Second
	case 1:
		return 5 * time.Second
	case 2:
		return 30 * time.Second
	case 3:
		return 2 * time.Minute
	default:
		return 10 * time.Minute
	}
}

// ParseBackoff reads a comma-separated list of waits, e.g. "1s,5s,30s":
// attempt n waits the n-th, and later attempts the last. An empty string
// is DefaultBackoff. Services expose it as configuration so a demo can
// dead-letter in seconds rather than minutes.
func ParseBackoff(s string) (BackoffFunc, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return DefaultBackoff, nil
	}
	parts := strings.Split(s, ",")
	steps := make([]time.Duration, 0, len(parts))
	for _, p := range parts {
		d, err := time.ParseDuration(strings.TrimSpace(p))
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("eventbus: backoff %q: want positive durations such as 1s,5s,30s", s)
		}
		steps = append(steps, d)
	}
	return func(attempt int) time.Duration {
		return steps[min(max(attempt, 0), len(steps)-1)]
	}, nil
}

func (c *Config) applyDefaults() {
	if c.OutboxPollInterval == 0 {
		c.OutboxPollInterval = 100 * time.Millisecond
	}
	if c.OutboxBatchSize == 0 {
		c.OutboxBatchSize = 50
	}
	if c.OutboxMaxAttempts == 0 {
		c.OutboxMaxAttempts = 5
	}
	if c.OutboxBackoff == nil {
		c.OutboxBackoff = DefaultBackoff
	}
	if c.ProcessedEventsRetention == 0 {
		c.ProcessedEventsRetention = 30 * 24 * time.Hour
	}
	if c.OutboxCleanupRetention == 0 {
		c.OutboxCleanupRetention = 7 * 24 * time.Hour
	}
	if c.CleanupInterval == 0 {
		c.CleanupInterval = 1 * time.Hour
	}
}

func (c *Config) validate() error {
	if c.NATSURL == "" {
		return fmt.Errorf("eventbus: NATSURL required")
	}
	if c.ServiceName == "" {
		return fmt.Errorf("eventbus: ServiceName required")
	}
	if c.Validator == nil {
		return fmt.Errorf("eventbus: Validator required (use NoopValidator{} if needed)")
	}
	return nil
}
