package eventbus

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
)

// TestClient is a wrapper for integration tests. Services use it to spin
// up an eventbus against a test Postgres and NATS, with sensible defaults
// and faster poll intervals.
type TestClient struct {
	*Client
	t testing.TB
}

// NewTestClient creates a Client with aggressive poll intervals and a
// NoopValidator by default. Caller must provide a connected pgxpool and
// NATS URL (typically from testcontainers).
func NewTestClient(t testing.TB, ctx context.Context, natsURL string, db *pgxpool.Pool, streams ...StreamConfig) *TestClient {
	t.Helper()
	c, err := NewClient(ctx, Config{
		NATSURL:                  natsURL,
		ServiceName:              "yc-test",
		DB:                       db,
		Validator:                NoopValidator{},
		Streams:                  streams,
		OutboxPollInterval:       20 * time.Millisecond,
		OutboxBatchSize:          20,
		OutboxMaxAttempts:        3,
		OutboxBackoff:            func(int) time.Duration { return 50 * time.Millisecond },
		ProcessedEventsRetention: time.Minute,
		OutboxCleanupRetention:   time.Minute,
		CleanupInterval:          500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("eventbus.NewTestClient: %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.Close(shutdownCtx)
	})
	return &TestClient{Client: c, t: t}
}

// DefaultTestStream creates a permissive stream capturing every yc.*
// subject. Suitable for tests; production uses tighter stream configs.
func DefaultTestStream(name string) StreamConfig {
	return StreamConfig{
		Name:     name,
		Subjects: []string{"yc.>"},
		Storage:  jetstream.MemoryStorage,
		MaxAge:   10 * time.Minute,
	}
}