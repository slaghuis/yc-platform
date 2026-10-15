// Command eventload publishes identity.visitor.created.v1 at a fixed rate
// through yc-eventbus's outbox, as yc-identity does for every new visitor,
// and measures how the relay and the consumers keep up.
//
//	go run ./test/load/eventload -rate 1000 -duration 60s
//
// The outbox lives in its own database (-db), so the run measures the
// library's relay without competing with yc-identity's. Consumers named by
// -consumers (yc-analytics and yc-monitor by default) must drain the
// backlog within -drain-timeout after publishing stops.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
)

const outboxDDL = `
CREATE TABLE IF NOT EXISTS outbox (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       TEXT NOT NULL,
    subject         TEXT NOT NULL,
    envelope        JSONB NOT NULL,
    headers         JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    published_at    TIMESTAMPTZ,
    attempts        INT NOT NULL DEFAULT 0,
    last_error      TEXT,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_outbox_unpublished ON outbox (next_attempt_at) WHERE published_at IS NULL;
CREATE TABLE IF NOT EXISTS processed_events (
    event_id     UUID NOT NULL,
    consumer     TEXT NOT NULL,
    processed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (event_id, consumer)
);
TRUNCATE outbox;
`

type options struct {
	natsURL, dbURL string
	tenants        []string
	consumers      []string
	rate           int
	duration       time.Duration
	workers        int
	drainTimeout   time.Duration
	out            string
}

// Sample is the state once a second.
type Sample struct {
	At        float64          `json:"t_seconds"`
	Inserted  int64            `json:"inserted"`
	Unrelayed int64            `json:"outbox_pending"`
	Pending   map[string]int64 `json:"consumer_pending"`
}

// Report is the run's result, written to -out as JSON.
type Report struct {
	Rate            int                `json:"target_rate"`
	Duration        string             `json:"duration"`
	Inserted        int64              `json:"inserted"`
	InsertErrors    int64              `json:"insert_errors"`
	InsertRate      float64            `json:"insert_rate"`
	InsertP95Millis float64            `json:"insert_p95_ms"`
	RelayLagMillis  map[string]float64 `json:"relay_lag_ms"`
	RelayRate       float64            `json:"relay_rate"`
	RelayDrained    string             `json:"relay_drained_after_stop"`
	ConsumerDrained map[string]string  `json:"consumer_drained_after_stop"`
	Samples         []Sample           `json:"samples"`
}

func main() {
	var o options
	var tenants, consumers string
	flag.StringVar(&o.natsURL, "nats", envOr("NATS_URL", "nats://localhost:4222"), "NATS URL")
	flag.StringVar(&o.dbURL, "db", "postgres://postgres:devpass@localhost:5432/yc_load?sslmode=disable", "outbox database")
	flag.StringVar(&tenants, "tenants", "load-1,load-2,load-3", "tenants to spread events over")
	flag.StringVar(&consumers, "consumers", "yc-analytics-identity-visitor-created,yc-monitor-visitor-created",
		"durable consumers that must drain")
	flag.IntVar(&o.rate, "rate", 1000, "events per second")
	flag.DurationVar(&o.duration, "duration", 60*time.Second, "how long to publish")
	flag.IntVar(&o.workers, "workers", 32, "concurrent publishing transactions")
	flag.DurationVar(&o.drainTimeout, "drain-timeout", 5*time.Minute, "how long to wait for the backlog to drain")
	flag.StringVar(&o.out, "out", "", "write the JSON report here")
	flag.Parse()
	o.tenants = strings.Split(tenants, ",")
	o.consumers = slices.DeleteFunc(strings.Split(consumers, ","), func(s string) bool { return s == "" })

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	report, err := run(ctx, o)
	if err != nil {
		fmt.Fprintln(os.Stderr, "eventload:", err)
		os.Exit(1)
	}
	b, _ := json.MarshalIndent(report, "", "  ")
	if o.out != "" {
		if err := os.WriteFile(o.out, b, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "eventload:", err)
			os.Exit(1)
		}
	}
	summary(report)
}

func run(ctx context.Context, o options) (*Report, error) {
	db, err := pgxpool.New(ctx, o.dbURL+"&pool_max_conns="+fmt.Sprint(o.workers+8))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if _, err := db.Exec(ctx, outboxDDL); err != nil {
		return nil, fmt.Errorf("outbox schema: %w", err)
	}
	shutdown, err := telemetry.Init(ctx, telemetry.Config{ServiceName: "yc-load", LogLevel: "warn"})
	if err != nil {
		return nil, err
	}
	defer func() { _ = shutdown(context.WithoutCancel(ctx)) }()
	validator, err := events.NewValidator()
	if err != nil {
		return nil, err
	}
	bus, err := eventbus.NewClient(ctx, eventbus.Config{
		NATSURL: o.natsURL, ServiceName: "yc-identity", DB: db, Validator: validator,
		Streams: []eventbus.StreamConfig{eventbus.IdentityStream()},
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = bus.Close(context.WithoutCancel(ctx)) }()
	nc, err := nats.Connect(o.natsURL)
	if err != nil {
		return nil, err
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, err
	}
	pending := func() map[string]int64 {
		m := make(map[string]int64, len(o.consumers))
		for _, name := range o.consumers {
			c, err := js.Consumer(ctx, eventbus.IdentityStream().Name, name)
			if err != nil {
				m[name] = -1
				continue
			}
			info, err := c.Info(ctx)
			if err != nil {
				m[name] = -1
				continue
			}
			//nolint:gosec // clamped to MaxInt64
			m[name] = int64(min(info.NumPending+uint64(info.NumAckPending), math.MaxInt64))
		}
		return m
	}
	for name, n := range pending() {
		if n < 0 {
			return nil, fmt.Errorf("consumer %s not found on %s; is the app stack up?", name, eventbus.IdentityStream().Name)
		}
	}

	r := &Report{Rate: o.rate, Duration: o.duration.String(), ConsumerDrained: map[string]string{}}
	var inserted, insertErrs atomic.Int64
	var latMu sync.Mutex
	var latencies []time.Duration

	start := time.Now()
	pubCtx, stopPub := context.WithTimeout(ctx, o.duration)
	defer stopPub()
	tokens := make(chan struct{}, o.rate)
	go func() {
		defer close(tokens)
		tick := time.NewTicker(time.Second / time.Duration(o.rate))
		defer tick.Stop()
		for {
			select {
			case <-pubCtx.Done():
				return
			case <-tick.C:
				select {
				case tokens <- struct{}{}:
				default: // workers are behind; the shortfall shows in insert_rate
				}
			}
		}
	}()
	var wg sync.WaitGroup
	for w := range o.workers {
		wg.Go(func() {
			for i := 0; ; i++ {
				if _, ok := <-tokens; !ok {
					return
				}
				t0 := time.Now()
				if err := publish(ctx, db, bus, o.tenants[(w+i)%len(o.tenants)]); err != nil {
					insertErrs.Add(1)
					continue
				}
				d := time.Since(t0)
				inserted.Add(1)
				latMu.Lock()
				latencies = append(latencies, d)
				latMu.Unlock()
			}
		})
	}

	unrelayed := func() int64 {
		var n int64
		_ = db.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE published_at IS NULL`).Scan(&n)
		return n
	}
	sample := func() Sample {
		return Sample{At: time.Since(start).Seconds(), Inserted: inserted.Load(), Unrelayed: unrelayed(), Pending: pending()}
	}
	every := time.NewTicker(time.Second)
	defer every.Stop()
	publishing := true
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	var stopped, deadline time.Time
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-done:
			done = nil
			publishing = false
			stopped = time.Now()
			deadline = stopped.Add(o.drainTimeout)
			r.InsertRate = float64(inserted.Load()) / stopped.Sub(start).Seconds()
			continue
		case <-every.C:
		}
		s := sample()
		r.Samples = append(r.Samples, s)
		fmt.Fprintf(os.Stderr, "t=%3.0fs inserted=%d outbox_pending=%d consumers=%v\n", s.At, s.Inserted, s.Unrelayed, s.Pending)
		if publishing {
			continue
		}
		if r.RelayDrained == "" && s.Unrelayed == 0 {
			r.RelayDrained = time.Since(stopped).Round(time.Second).String()
		}
		for name, n := range s.Pending {
			if _, ok := r.ConsumerDrained[name]; !ok && n == 0 && s.Unrelayed == 0 {
				r.ConsumerDrained[name] = time.Since(stopped).Round(time.Second).String()
			}
		}
		if r.RelayDrained != "" && len(r.ConsumerDrained) == len(o.consumers) {
			break
		}
		if time.Now().After(deadline) {
			for _, name := range o.consumers {
				if _, ok := r.ConsumerDrained[name]; !ok {
					r.ConsumerDrained[name] = fmt.Sprintf("not within %s (%d left)", o.drainTimeout, s.Pending[name])
				}
			}
			if r.RelayDrained == "" {
				r.RelayDrained = fmt.Sprintf("not within %s (%d left)", o.drainTimeout, s.Unrelayed)
			}
			break
		}
	}

	r.Inserted, r.InsertErrors = inserted.Load(), insertErrs.Load()
	slices.Sort(latencies)
	r.InsertP95Millis = millis(percentile(latencies, 0.95))
	r.RelayLagMillis = map[string]float64{}
	var first, last time.Time
	var p50, p95, p99, maxLag float64
	err = db.QueryRow(ctx, `
		SELECT min(created_at), max(published_at),
		       percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM published_at - created_at)) * 1000,
		       percentile_cont(0.95) WITHIN GROUP (ORDER BY extract(epoch FROM published_at - created_at)) * 1000,
		       percentile_cont(0.99) WITHIN GROUP (ORDER BY extract(epoch FROM published_at - created_at)) * 1000,
		       max(extract(epoch FROM published_at - created_at)) * 1000
		FROM outbox WHERE published_at IS NOT NULL`).Scan(&first, &last, &p50, &p95, &p99, &maxLag)
	if err != nil {
		return nil, fmt.Errorf("relay lag: %w", err)
	}
	r.RelayLagMillis = map[string]float64{"p50": p50, "p95": p95, "p99": p99, "max": maxLag}
	if d := last.Sub(first).Seconds(); d > 0 {
		r.RelayRate = float64(r.Inserted) / d
	}
	return r, nil
}

func publish(ctx context.Context, db *pgxpool.Pool, bus *eventbus.Client, tenant string) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	id := uuid.NewString()
	now := time.Now().UTC()
	err = bus.Publish(ctx, tx, eventbus.Event{
		EventType: events.TypeIdentityVisitorCreatedV1,
		TenantID:  tenant,
		Actor:     eventbus.Actor{Type: "visitor", ID: id},
		Payload:   events.IdentityVisitorCreatedV1{UserID: id, CreatedAt: now, TokenExpiresAt: now.Add(30 * 24 * time.Hour)},
	})
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func summary(r *Report) {
	fmt.Printf("\ntarget %d/s for %s\n", r.Rate, r.Duration)
	fmt.Printf("inserted %d (%.0f/s, %d errors), insert p95 %.1f ms\n", r.Inserted, r.InsertRate, r.InsertErrors, r.InsertP95Millis)
	fmt.Printf("relay %.0f/s, lag p50 %.0f ms, p95 %.0f ms, p99 %.0f ms, max %.0f ms; drained %s after publishing stopped\n",
		r.RelayRate, r.RelayLagMillis["p50"], r.RelayLagMillis["p95"], r.RelayLagMillis["p99"], r.RelayLagMillis["max"], r.RelayDrained)
	for name, d := range r.ConsumerDrained {
		fmt.Printf("consumer %s drained %s after publishing stopped\n", name, d)
	}
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[min(len(sorted)-1, int(float64(len(sorted))*p))]
}

func millis(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
