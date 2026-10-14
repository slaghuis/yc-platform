package eventbus

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/<org>/yc-platform/libs/yc-telemetry"
)

// Client is the eventbus client. Construct one per service via NewClient
// and keep it alive for the process lifetime.
type Client struct {
	cfg Config

	nc  *nats.Conn
	js  jetstream.JetStream
	db  *pgxpool.Pool

	validator Validator

	// Shutdown coordination.
	bgCtx    context.Context
	bgCancel context.CancelFunc
	bgWG     sync.WaitGroup

	// Metrics
	metricPublishTotal         any // see metrics.go
	metricConsumeTotal         any
	metricOutboxPending        any
	metricPublishDurationSec   any
	metricConsumeDurationSec   any
}

// NewClient connects to NATS, ensures required streams exist, and starts
// the outbox relay and cleanup goroutines.
func NewClient(ctx context.Context, cfg Config) (*Client, error) {
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	nc, err := nats.Connect(cfg.NATSURL, cfg.NATSOptions...)
	if err != nil {
		return nil, fmt.Errorf("eventbus: nats connect: %w", err)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("eventbus: jetstream: %w", err)
	}

	for _, sc := range cfg.Streams {
		if err := ensureStream(ctx, js, sc); err != nil {
			nc.Close()
			return nil, fmt.Errorf("eventbus: ensure stream %q: %w", sc.Name, err)
		}
	}

	bgCtx, bgCancel := context.WithCancel(context.Background())
	c := &Client{
		cfg:       cfg,
		nc:        nc,
		js:        js,
		db:        cfg.DB,
		validator: cfg.Validator,
		bgCtx:     bgCtx,
		bgCancel:  bgCancel,
	}
	c.initMetrics()

	c.bgWG.Add(2)
	go c.runRelay()
	go c.runCleanup()

	telemetry.LoggerFromContext(ctx).Info("eventbus client started",
		"nats_url", cfg.NATSURL,
		"service", cfg.ServiceName,
		"streams", len(cfg.Streams),
	)

	return c, nil
}

// Close drains and shuts down background goroutines and the NATS
// connection. Safe to call multiple times.
func (c *Client) Close(ctx context.Context) error {
	c.bgCancel()

	done := make(chan struct{})
	go func() {
		c.bgWG.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}

	if err := c.nc.Drain(); err != nil {
		return fmt.Errorf("eventbus: nats drain: %w", err)
	}
	return nil
}

func ensureStream(ctx context.Context, js jetstream.JetStream, sc StreamConfig) error {
	if sc.Name == "" || len(sc.Subjects) == 0 {
		return fmt.Errorf("stream must have Name and at least one Subject")
	}

	cfg := jetstream.StreamConfig{
		Name:      sc.Name,
		Subjects:  sc.Subjects,
		Storage:   sc.Storage,
		MaxAge:    sc.MaxAge,
		MaxBytes:  sc.MaxBytes,
		Retention: jetstream.LimitsPolicy,
	}

	_, err := js.CreateOrUpdateStream(ctx, cfg)
	return err
}