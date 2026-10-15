package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
)

// SubscribeOpts configures a durable JetStream consumer.
type SubscribeOpts struct {
	// Subject is the subscription pattern, e.g. "yc.*.order.placed.v1".
	Subject string

	// DurableName is the JetStream durable consumer name. Must be unique
	// per subscriber role. Convention: "<service>-<handler-name>".
	DurableName string

	// MaxRetries before a message goes to DLQ. Default: 5.
	MaxRetries int

	// AckWait is how long NATS waits for an ack before redelivering.
	// Default: 30s.
	AckWait time.Duration

	// Concurrency is the number of messages handled at once. Default: 1.
	// Above 1 messages complete out of order, so only handlers that do not
	// depend on order (counters, idempotent inserts) may raise it.
	Concurrency int
}

// pullBuffer is the minimum number of messages a subscription keeps
// buffered, so that handling one does not wait for a round trip to fetch
// the next. Buffered messages are still handled in order.
const pullBuffer = 16

func (o *SubscribeOpts) applyDefaults() {
	if o.MaxRetries == 0 {
		o.MaxRetries = 5
	}
	if o.AckWait == 0 {
		o.AckWait = 30 * time.Second
	}
	if o.Concurrency == 0 {
		o.Concurrency = 1
	}
}

func (o *SubscribeOpts) validate() error {
	if o.Subject == "" {
		return fmt.Errorf("subscribe: Subject required")
	}
	if o.DurableName == "" {
		return fmt.Errorf("subscribe: DurableName required")
	}
	return nil
}

// ConsumedEvent is handed to subscriber handlers.
type ConsumedEvent struct {
	// Envelope is the parsed envelope.
	Envelope Envelope

	// Payload is the raw payload JSON. The handler unmarshals into a
	// typed struct.
	Payload json.RawMessage

	// RawEnvelope is the full envelope JSON, useful for re-publishing or
	// forensic logging.
	RawEnvelope []byte
}

// Handler processes a consumed event. See docs/standards/event-conventions.md
// §8 for the error-handling contract:
//
//   - nil error             → ack
//   - ErrPoison (wrapped)   → DLQ immediately
//   - any other error       → retry per backoff, then DLQ
type Handler func(ctx context.Context, evt ConsumedEvent) error

// Subscribe creates a durable JetStream consumer and dispatches messages
// to the handler. Subscribe returns once the consumer is created; the
// client keeps the subscription active until Close.
func (c *Client) Subscribe(ctx context.Context, opts SubscribeOpts, h Handler) error {
	if c.db == nil {
		return errNoDB
	}
	opts.applyDefaults()
	if err := opts.validate(); err != nil {
		return err
	}
	//nolint:contextcheck // the subscription outlives ctx and stops with the client's background context
	return c.startConsumer(ctx, opts, func(msg jetstream.Msg) { c.handleMessage(opts, h, msg) })
}

// startConsumer creates or updates the durable consumer and dispatches its
// messages to handle until the client closes.
func (c *Client) startConsumer(ctx context.Context, opts SubscribeOpts, handle func(jetstream.Msg)) error {
	// JetStream requires us to pick a stream; we discover it by subject.
	stream, err := c.streamForSubject(ctx, opts.Subject)
	if err != nil {
		return fmt.Errorf("subscribe: find stream for %q: %w", opts.Subject, err)
	}

	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       opts.DurableName,
		FilterSubject: opts.Subject,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       opts.AckWait,
		MaxDeliver:    opts.MaxRetries + 1, // initial + retries
		DeliverPolicy: jetstream.DeliverAllPolicy,
	})
	if err != nil {
		return fmt.Errorf("subscribe: create consumer %q: %w", opts.DurableName, err)
	}

	iter, err := cons.Messages(jetstream.PullMaxMessages(max(2*opts.Concurrency, pullBuffer)))
	if err != nil {
		return fmt.Errorf("subscribe: messages iter: %w", err)
	}

	c.bgWG.Add(1)
	go c.runSubscription(opts, iter, handle)

	telemetry.LoggerFromContext(ctx).Info("eventbus subscription active",
		"subject", opts.Subject,
		"durable", opts.DurableName,
	)
	return nil
}

func (c *Client) streamForSubject(ctx context.Context, subject string) (jetstream.Stream, error) {
	// JetStream exposes stream lookup by subject.
	stream, err := c.js.StreamNameBySubject(ctx, subject)
	if err != nil {
		return nil, err
	}
	return c.js.Stream(ctx, stream)
}

func (c *Client) runSubscription(opts SubscribeOpts, iter jetstream.MessagesContext, handle func(jetstream.Msg)) {
	defer c.bgWG.Done()
	defer iter.Stop()
	stop := context.AfterFunc(c.bgCtx, iter.Stop)
	defer stop()

	slots := make(chan struct{}, max(opts.Concurrency, 1))
	var handling sync.WaitGroup
	defer handling.Wait()
	for {
		msg, err := iter.Next()
		if err != nil {
			if errors.Is(err, jetstream.ErrMsgIteratorClosed) {
				return
			}
			if c.bgCtx.Err() != nil {
				return
			}
			telemetry.LoggerFromContext(c.bgCtx).Warn("subscription iterator error",
				"durable", opts.DurableName, "err", err)
			continue
		}
		if opts.Concurrency <= 1 {
			handle(msg)
			continue
		}
		slots <- struct{}{}
		handling.Go(func() {
			defer func() { <-slots }()
			handle(msg)
		})
	}
}

func (c *Client) handleMessage(opts SubscribeOpts, h Handler, msg jetstream.Msg) {
	deliveries := 1
	if meta, err := msg.Metadata(); err == nil {
		deliveries = int(meta.NumDelivered) //nolint:gosec // bounded by the consumer's MaxDeliver
	}

	ctx, span := telemetry.StartSpan(extractTrace(c.bgCtx, msg.Headers()), "event.consume",
		attribute.String("event.subject", msg.Subject()),
		attribute.String("durable", opts.DurableName),
		attribute.Int("delivery_count", deliveries),
	)
	defer span.End()

	var env Envelope
	if err := json.Unmarshal(msg.Data(), &env); err != nil {
		c.consumeOutcomeDLQ(ctx, opts, msg, nil, deliveries, fmt.Errorf("unmarshal envelope: %w", err), "poison")
		return
	}

	span.SetName("event.consume " + env.EventType)
	span.SetAttributes(
		attribute.String("event.id", env.EventID),
		attribute.String("event.type", env.EventType),
		attribute.String("tenant_id", env.TenantID),
	)

	if err := CheckSubjectTenant(msg.Subject(), env.TenantID); err != nil {
		c.consumeOutcomeDLQ(ctx, opts, msg, &env, deliveries, err, "poison")
		return
	}
	ctx = withEnvelope(ctx, &env)
	logger := telemetry.LoggerFromContext(ctx)

	// Dedup check.
	alreadyProcessed, err := c.isAlreadyProcessed(ctx, env.EventID, opts.DurableName)
	if err != nil {
		logger.Error("dedup lookup failed", "err", err)
		// Return without ack so NATS redelivers.
		_ = msg.Nak()
		return
	}
	if alreadyProcessed {
		_ = msg.Ack()
		c.consumeCounter().Add(ctx, 1,
			metric.WithAttributes(
				attribute.String("outcome", "dedup"),
				attribute.String("durable", opts.DurableName),
			))
		return
	}

	// Validate payload before handling.
	if err := c.validator.ValidatePayload(ctx, &env); err != nil {
		c.consumeOutcomeDLQ(ctx, opts, msg, &env, deliveries, err, "poison")
		return
	}

	// Invoke handler.
	start := time.Now()
	herr := h(ctx, ConsumedEvent{
		Envelope:    env,
		Payload:     env.Payload,
		RawEnvelope: msg.Data(),
	})
	c.consumeDuration().Record(ctx, time.Since(start).Seconds(),
		metric.WithAttributes(attribute.String("event.type", env.EventType)))

	if herr == nil {
		if err := c.markProcessed(ctx, env.EventID, opts.DurableName); err != nil {
			logger.Error("mark processed", "err", err)
			// Still ack — reprocessing on redelivery is safe because
			// handler already succeeded. The dedup table is defence
			// in depth.
		}
		_ = msg.Ack()
		c.consumeCounter().Add(ctx, 1,
			metric.WithAttributes(
				attribute.String("outcome", "ok"),
				attribute.String("durable", opts.DurableName),
			))
		return
	}

	if IsPoison(herr) {
		c.consumeOutcomeDLQ(ctx, opts, msg, &env, deliveries, herr, "poison")
		return
	}

	// Retriable: let NATS redeliver, unless we've exceeded MaxRetries.
	if deliveries >= opts.MaxRetries+1 {
		c.consumeOutcomeDLQ(ctx, opts, msg, &env, deliveries, herr, "exhausted")
		return
	}

	telemetry.RecordSpanError(span, herr)
	logger.Warn("handler failed, will retry",
		"event_id", env.EventID,
		"event_type", env.EventType,
		"attempt", deliveries,
		"err", herr,
	)
	_ = msg.NakWithDelay(c.cfg.OutboxBackoff(deliveries))
	c.consumeCounter().Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("outcome", "retry"),
			attribute.String("durable", opts.DurableName),
		))
}

func (c *Client) consumeOutcomeDLQ(
	ctx context.Context,
	opts SubscribeOpts,
	msg jetstream.Msg,
	env *Envelope,
	deliveries int,
	cause error,
	reason string,
) {
	logger := telemetry.LoggerFromContext(ctx)

	// A tenant's DLQ only receives events whose envelope tenant is valid and
	// matches the delivery subject; anything else goes to the platform DLQ
	// so it is never replayed into the wrong tenant.
	tenantID := PlatformTenantID
	eventType := ""
	if env != nil {
		if CheckSubjectTenant(msg.Subject(), env.TenantID) == nil {
			tenantID = env.TenantID
		}
		eventType = env.EventType
	}

	// If we couldn't even parse the envelope, we don't know the domain.
	domain := "unknown"
	if eventType != "" {
		if d, err := DomainOfEventType(eventType); err == nil {
			domain = d
		}
	}

	dlqSubject, err := SubjectForDLQ(tenantID, domain)
	if err != nil {
		logger.Error("DLQ subject", "err", err)
		_ = msg.Term()
		return
	}

	var original Envelope
	if env != nil {
		original = *env
	} else {
		// Best-effort: keep the raw bytes as a JSON string payload, since
		// they may not be valid JSON, so the dead letter can be inspected.
		raw, _ := json.Marshal(string(msg.Data()))
		original = Envelope{
			EventID:       newEventID(),
			EventType:     "unknown.unparsed.v1",
			EventTime:     time.Now().UTC(),
			TenantID:      PlatformTenantID,
			SourceService: "unknown",
			Payload:       raw,
		}
		cause = fmt.Errorf("subject %s: %w", msg.Subject(), cause)
	}

	dlqEnv := DLQEnvelope{
		OriginalEvent: original,
		DLQMetadata: DLQMetadata{
			Service:       c.cfg.ServiceName,
			Consumer:      opts.DurableName,
			Attempts:      deliveries,
			FirstFailure:  original.EventTime,
			LastFailure:   time.Now().UTC(),
			LastError:     cause.Error(),
			FailureReason: reason,
		},
	}

	payload, err := json.Marshal(dlqEnv)
	if err != nil {
		logger.Error("DLQ marshal", "err", err)
		_ = msg.Term()
		return
	}

	if _, err := c.js.PublishMsg(ctx, newMsg(dlqSubject, payload, traceHeaders(ctx))); err != nil {
		logger.Error("DLQ publish", "subject", dlqSubject, "err", err)
		// Can't DLQ. Term the message to avoid infinite redelivery.
		_ = msg.Term()
		return
	}

	_ = msg.Ack()
	c.consumeCounter().Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("outcome", "dlq"),
			attribute.String("durable", opts.DurableName),
			attribute.String("reason", reason),
		))

	logger.Warn("event sent to DLQ",
		"durable", opts.DurableName,
		"subject", dlqSubject,
		"reason", reason,
		"err", cause,
	)
}

func (c *Client) isAlreadyProcessed(ctx context.Context, eventID, consumer string) (bool, error) {
	var exists bool
	err := c.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM processed_events
			WHERE event_id = $1 AND consumer = $2
		)
	`, eventID, consumer).Scan(&exists)
	return exists, err
}

func (c *Client) markProcessed(ctx context.Context, eventID, consumer string) error {
	_, err := c.db.Exec(ctx, `
		INSERT INTO processed_events (event_id, consumer, processed_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (event_id, consumer) DO NOTHING
	`, eventID, consumer)
	if err != nil {
		// Classify as transient so callers can decide whether to retry;
		// the caller already acked, so it only affects dedup state.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			return fmt.Errorf("processed_events insert: %s: %w", pgErr.Code, err)
		}
		return err
	}
	return nil
}
