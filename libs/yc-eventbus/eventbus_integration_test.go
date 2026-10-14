//go:build integration

package eventbus_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	testkit "github.com/slaghuis/yc-platform/libs/yc-testkit"
)

const (
	waitFor = 10 * time.Second
	tick    = 20 * time.Millisecond
	tenant  = "shop-a"
)

type harness struct {
	natsURL string
	db      *pgxpool.Pool
	js      jetstream.JetStream
	streams []eventbus.StreamConfig
}

func (h *harness) client(t *testing.T, mutate ...func(*eventbus.Config)) *eventbus.Client {
	t.Helper()
	cfg := eventbus.Config{
		NATSURL:            h.natsURL,
		ServiceName:        "yc-test",
		DB:                 h.db,
		Validator:          eventbus.NoopValidator{},
		Streams:            h.streams,
		OutboxPollInterval: 20 * time.Millisecond,
		OutboxBackoff:      func(int) time.Duration { return 50 * time.Millisecond },
	}
	for _, m := range mutate {
		m(&cfg)
	}
	c, err := eventbus.NewClient(context.Background(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		assert.NoError(t, c.Close(ctx))
	})
	return c
}

func (h *harness) publish(t *testing.T, ctx context.Context, c *eventbus.Client, evt eventbus.Event) {
	t.Helper()
	tx, err := h.db.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, c.Publish(ctx, tx, evt))
	require.NoError(t, tx.Commit(ctx))
}

// dlq waits for the latest dead letter on subject.
func (h *harness) dlq(t *testing.T, subject string) eventbus.DLQEnvelope {
	t.Helper()
	stream, err := h.js.Stream(context.Background(), eventbus.DLQStreamName)
	require.NoError(t, err)
	var raw *jetstream.RawStreamMsg
	require.Eventually(t, func() bool {
		raw, err = stream.GetLastMsgForSubject(context.Background(), subject)
		return err == nil
	}, waitFor, tick, "no dead letter on %s", subject)
	var out eventbus.DLQEnvelope
	require.NoError(t, json.Unmarshal(raw.Data, &out))
	return out
}

func (h *harness) outboxCount(t *testing.T, eventType, where string) int {
	t.Helper()
	var n int
	require.NoError(t, h.db.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox WHERE envelope->>'event_type' = $1 AND `+where, eventType).Scan(&n))
	return n
}

func event(eventType string, payload any) eventbus.Event {
	return eventbus.Event{
		EventType: eventType,
		TenantID:  tenant,
		Actor:     eventbus.Actor{Type: "visitor", ID: "v-1"},
		Payload:   payload,
	}
}

func rawEnvelope(t *testing.T, eventType, tenantID string) []byte {
	t.Helper()
	b, err := json.Marshal(eventbus.Envelope{
		EventID:       uuid.NewString(),
		EventType:     eventType,
		EventTime:     time.Now().UTC(),
		TenantID:      tenantID,
		Actor:         eventbus.Actor{Type: "service", ID: "yc-test"},
		CorrelationID: uuid.NewString(),
		SourceService: "yc-test",
		SchemaVersion: 1,
		Payload:       json.RawMessage(`{}`),
	})
	require.NoError(t, err)
	return b
}

func TestEventbus(t *testing.T) {
	ctx := context.Background()

	telemetry.InitForTest(t, "yc-test")

	natsURL := testkit.NATS(t)
	db, err := pgxpool.New(ctx, testkit.Postgres(t))
	require.NoError(t, err)
	t.Cleanup(db.Close)
	schema, err := os.ReadFile("schema.sql")
	require.NoError(t, err)
	_, err = db.Exec(ctx, string(schema))
	require.NoError(t, err)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	h := &harness{
		natsURL: natsURL,
		db:      db,
		js:      js,
		streams: []eventbus.StreamConfig{
			eventbus.DLQStream(),
			eventbus.DomainStream("test", time.Hour),
		},
	}

	t.Run("publish relays and consumes with context", func(t *testing.T) {
		const et = "test.thing.happened.v1"
		c := h.client(t)

		type got struct {
			evt     eventbus.ConsumedEvent
			auth    authctx.AuthContext
			traceID trace.TraceID
		}
		ch := make(chan got, 1)
		require.NoError(t, c.Subscribe(ctx, eventbus.SubscribeOpts{
			Subject: "yc.*." + et, DurableName: "relay-consumer",
		}, func(ctx context.Context, evt eventbus.ConsumedEvent) error {
			a, _ := authctx.FromContext(ctx)
			ch <- got{evt, a, trace.SpanContextFromContext(ctx).TraceID()}
			return nil
		}))

		corr := uuid.NewString()
		reqCtx, span := otel.Tracer("test").Start(authctx.WithCorrelationID(ctx, corr), "request")
		h.publish(t, reqCtx, c, event(et, map[string]string{"k": "v"}))
		span.End()

		var g got
		select {
		case g = <-ch:
		case <-time.After(waitFor):
			t.Fatal("event not consumed")
		}
		assert.Equal(t, tenant, g.evt.Envelope.TenantID)
		assert.Equal(t, corr, g.evt.Envelope.CorrelationID)
		assert.Equal(t, "yc-test", g.evt.Envelope.SourceService)
		assert.Equal(t, 1, g.evt.Envelope.SchemaVersion)
		assert.JSONEq(t, `{"k":"v"}`, string(g.evt.Payload))
		assert.Equal(t, tenant, g.auth.TenantID)
		assert.Equal(t, "v-1", g.auth.ActorID)
		assert.Equal(t, corr, g.auth.CorrelationID)
		assert.Equal(t, span.SpanContext().TraceID(), g.traceID, "consumer joins the publisher's trace")

		assert.Eventually(t, func() bool {
			return h.outboxCount(t, et, "published_at IS NOT NULL") == 1
		}, waitFor, tick)
		var processed int
		require.NoError(t, db.QueryRow(ctx,
			`SELECT count(*) FROM processed_events WHERE event_id = $1 AND consumer = 'relay-consumer'`,
			g.evt.Envelope.EventID).Scan(&processed))
		assert.Equal(t, 1, processed)
	})

	t.Run("published events inherit causation from the consumed event", func(t *testing.T) {
		const first, second = "test.chain.started.v1", "test.chain.continued.v1"
		c := h.client(t)

		require.NoError(t, c.Subscribe(ctx, eventbus.SubscribeOpts{
			Subject: "yc.*." + first, DurableName: "chain-step",
		}, func(ctx context.Context, _ eventbus.ConsumedEvent) error {
			return pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
				return c.Publish(ctx, tx, event(second, map[string]string{}))
			})
		}))
		done := make(chan eventbus.Envelope, 1)
		require.NoError(t, c.Subscribe(ctx, eventbus.SubscribeOpts{
			Subject: "yc.*." + second, DurableName: "chain-end",
		}, func(_ context.Context, evt eventbus.ConsumedEvent) error {
			done <- evt.Envelope
			return nil
		}))

		h.publish(t, ctx, c, event(first, map[string]string{}))
		var firstEnv eventbus.Envelope
		require.NoError(t, db.QueryRow(ctx,
			`SELECT envelope FROM outbox WHERE envelope->>'event_type' = $1`, first).Scan(&firstEnv))

		select {
		case env := <-done:
			assert.Equal(t, firstEnv.EventID, env.CausationID)
			assert.Equal(t, firstEnv.CorrelationID, env.CorrelationID)
		case <-time.After(waitFor):
			t.Fatal("chained event not consumed")
		}
	})

	t.Run("rollback leaves no outbox row", func(t *testing.T) {
		const et = "test.rollback.happened.v1"
		c := h.client(t)
		tx, err := db.Begin(ctx)
		require.NoError(t, err)
		require.NoError(t, c.Publish(ctx, tx, event(et, map[string]string{})))
		require.NoError(t, tx.Rollback(ctx))
		assert.Zero(t, h.outboxCount(t, et, "true"))
	})

	t.Run("duplicate event is handled once per consumer", func(t *testing.T) {
		const et = "test.dedup.happened.v1"
		c := h.client(t)
		var a, b atomic.Int32
		for name, n := range map[string]*atomic.Int32{"dedup-a": &a, "dedup-b": &b} {
			require.NoError(t, c.Subscribe(ctx, eventbus.SubscribeOpts{
				Subject: "yc.*." + et, DurableName: name,
			}, func(context.Context, eventbus.ConsumedEvent) error {
				n.Add(1)
				return nil
			}))
		}

		msg := rawEnvelope(t, et, tenant)
		for range 2 {
			_, err := js.Publish(ctx, "yc."+tenant+"."+et, msg)
			require.NoError(t, err)
		}
		require.Eventually(t, func() bool { return a.Load() >= 1 && b.Load() >= 1 }, waitFor, tick)
		time.Sleep(300 * time.Millisecond)
		assert.EqualValues(t, 1, a.Load())
		assert.EqualValues(t, 1, b.Load())
	})

	t.Run("poison goes straight to the tenant DLQ", func(t *testing.T) {
		const et = "test.poison.happened.v1"
		c := h.client(t)
		var calls atomic.Int32
		require.NoError(t, c.Subscribe(ctx, eventbus.SubscribeOpts{
			Subject: "yc.*." + et, DurableName: "poison-consumer",
		}, func(context.Context, eventbus.ConsumedEvent) error {
			calls.Add(1)
			return errors.Join(eventbus.ErrPoison, errors.New("bad payload"))
		}))
		h.publish(t, ctx, c, event(et, map[string]string{}))

		d := h.dlq(t, "yc."+tenant+".test.dlq.v1")
		assert.Equal(t, "poison", d.DLQMetadata.FailureReason)
		assert.Equal(t, "poison-consumer", d.DLQMetadata.Consumer)
		assert.Equal(t, et, d.OriginalEvent.EventType)
		assert.EqualValues(t, 1, calls.Load())
	})

	t.Run("retries exhaust into the DLQ", func(t *testing.T) {
		const et = "test.flaky.happened.v1"
		c := h.client(t)
		var calls atomic.Int32
		require.NoError(t, c.Subscribe(ctx, eventbus.SubscribeOpts{
			Subject: "yc.*." + et, DurableName: "flaky-consumer", MaxRetries: 2,
		}, func(context.Context, eventbus.ConsumedEvent) error {
			calls.Add(1)
			return errors.New("downstream unavailable")
		}))
		evt := event(et, map[string]string{})
		evt.TenantID = "shop-flaky"
		h.publish(t, ctx, c, evt)

		d := h.dlq(t, "yc.shop-flaky.test.dlq.v1")
		assert.Equal(t, et, d.OriginalEvent.EventType)
		assert.Equal(t, "exhausted", d.DLQMetadata.FailureReason)
		assert.Equal(t, 3, d.DLQMetadata.Attempts)
		assert.EqualValues(t, 3, calls.Load())
	})

	t.Run("malformed envelope goes to the platform DLQ", func(t *testing.T) {
		const et = "test.garbage.happened.v1"
		c := h.client(t)
		require.NoError(t, c.Subscribe(ctx, eventbus.SubscribeOpts{
			Subject: "yc.*." + et, DurableName: "garbage-consumer",
		}, func(context.Context, eventbus.ConsumedEvent) error {
			t.Error("handler must not see a malformed envelope")
			return nil
		}))
		_, err := js.Publish(ctx, "yc."+tenant+"."+et, []byte("not json"))
		require.NoError(t, err)

		d := h.dlq(t, "yc._platform.unknown.dlq.v1")
		assert.Equal(t, "poison", d.DLQMetadata.FailureReason)
		assert.Equal(t, "garbage-consumer", d.DLQMetadata.Consumer)
	})

	t.Run("tenant mismatch goes to the platform DLQ", func(t *testing.T) {
		const et = "test.mismatch.happened.v1"
		c := h.client(t)
		require.NoError(t, c.Subscribe(ctx, eventbus.SubscribeOpts{
			Subject: "yc.*." + et, DurableName: "mismatch-consumer",
		}, func(context.Context, eventbus.ConsumedEvent) error {
			t.Error("handler must not see another tenant's event")
			return nil
		}))
		_, err := js.Publish(ctx, "yc."+tenant+"."+et, rawEnvelope(t, et, "shop-b"))
		require.NoError(t, err)

		d := h.dlq(t, "yc._platform.test.dlq.v1")
		assert.Equal(t, "poison", d.DLQMetadata.FailureReason)
		assert.Equal(t, "shop-b", d.OriginalEvent.TenantID)
	})

	t.Run("outbox rows survive a crash and relay on restart", func(t *testing.T) {
		const et = "test.crash.happened.v1"
		crashed := h.client(t, func(c *eventbus.Config) { c.DisableRelay = true })
		h.publish(t, ctx, crashed, event(et, map[string]string{}))
		time.Sleep(200 * time.Millisecond)
		require.Equal(t, 1, h.outboxCount(t, et, "published_at IS NULL"), "relay disabled, row must stay pending")
		require.NoError(t, crashed.Close(ctx))

		h.client(t)
		assert.Eventually(t, func() bool {
			return h.outboxCount(t, et, "published_at IS NOT NULL") == 1
		}, waitFor, tick)
		stream, err := js.Stream(ctx, "YC_TEST")
		require.NoError(t, err)
		_, err = stream.GetLastMsgForSubject(ctx, "yc."+tenant+"."+et)
		assert.NoError(t, err)
	})

	t.Run("publish-only client publishes direct", func(t *testing.T) {
		const et = "test.direct.happened.v1"
		c := h.client(t, func(c *eventbus.Config) { c.DB = nil })
		require.NoError(t, c.Ping(ctx))
		require.NoError(t, c.PublishDirect(ctx, event(et, map[string]string{})))

		stream, err := js.Stream(ctx, "YC_TEST")
		require.NoError(t, err)
		_, err = stream.GetLastMsgForSubject(ctx, "yc."+tenant+"."+et)
		assert.NoError(t, err)

		tx, err := db.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()
		assert.Error(t, c.Publish(ctx, tx, event(et, map[string]string{})))
		assert.Error(t, c.Subscribe(ctx, eventbus.SubscribeOpts{Subject: "yc.*." + et, DurableName: "x"},
			func(context.Context, eventbus.ConsumedEvent) error { return nil }))
	})
}
