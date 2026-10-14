package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/slaghuis/yc-platform/libs/yc-telemetry"
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

	// Concurrency is the number of messages fetched at a time. Default: 1.
	Concurrency int
}

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
	opts.applyDefaults()
	if err := opts.validate(); err != nil {
		return err
	}

	// Find or create a consumer. JetStream requires us to pick a stream;
	// we discover it by subject.
	stream, err := c.streamForSubject(ctx, opts.Subject)
	if err != nil {
		return fmt.Errorf("subscribe: find stream for %q: %w", opts.Subject, err)
	}

	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:        opts.DurableName,
		FilterSubject:  opts.Subject,
		AckPolicy:      jetstream.AckExplicitPolicy,
		AckWait:        opts.AckWait,
		MaxDeliver:     opts.MaxRetries + 1, // initial + retries
		DeliverPolicy:  jetstream.DeliverAllPolicy,
	})
	if err != nil {
		return fmt.Errorf("subscribe: create consumer %q: %w", opts.DurableName, err)
	}

	iter, err := cons.Messages(jetstream.PullMaxMessages(opts.Concurrency))
	if err != nil {
		return fmt.Errorf("subscribe: messages iter: %w", err)
	}

	c.bgWG.Add(1)
	go c.runSubscription(opts, iter, h)

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

func (c *Client) runSubscription(opts SubscribeOpts, iter jetstream.MessagesContext, h Handler) {
	defer c.bgWG.Done()
	defer iter.Stop()

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
		c.handleMessage(opts, h, msg)
	}
}

func (c *Client) handleMessage(opts SubscribeOpts, h Handler, msg jetstream.Msg) {
	meta, _ := msg.Metadata()
	deliveries := int(meta.NumDelivered)

	ctx, span := telemetry.StartSpan(c.bgCtx, "eventbus.consume",
		attribute.String("event.subject", msg.Subject()),
		attribute.String("durable", opts.DurableName),
		attribute.Int("delivery_count", deliveries),
	)
	defer span.End()

	logger := telemetry.LoggerFromContext(ctx)

	var env Envelope
	if err := json.Unmarshal(msg.Data(), &env); err != nil {
		c.consumeOutcomeDLQ(ctx, opts, msg, nil, deliveries, fmt.Errorf("unmarshal envelope: %w", err), "poison")
		return
	}

	span.SetAttributes(
		attribute.String("event.id", env.EventID),
		attribute.String("event.type", env.EventType),
		attribute.String("tenant_id", env.TenantID),
	)

	if err := CheckSubjectTenant(msg.Subject(), env.TenantID); err != nil {
		c.consumeOutcomeDLQ(ctx, opts, msg, &env, deliveries, err, "poison")
		return
	}

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
			// handler already succeeded. The dedup table is defense
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
		// Best-effort: wrap raw bytes as an opaque payload so the
		// envelope schema stays satisfied for forensics.
		original = Envelope{
			EventID:   "unparsed-" + msg.Subject(),
			EventType: "unknown.unknown.v0",
			EventTime: time.Now().UTC(),
			TenantID:  PlatformTenantID,
			Payload:   msg.Data(),
		}
	}

	dlqEnv := DLQEnvelope{
		OriginalEvent: original,
		DLQMetadata: DLQMetadata{
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

	if _, err := c.js.Publish(ctx, dlqSubject, payload); err != nil {
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