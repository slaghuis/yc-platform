package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
)

// Publish writes an event to the outbox within the caller's transaction.
// The event is published to NATS by the relay goroutine; this call does
// not touch NATS.
//
// The tx must be live; this method does not commit. The current trace
// context is stored with the row so the relay's publish and the consumer's
// handling join the caller's trace.
func (c *Client) Publish(ctx context.Context, tx pgx.Tx, evt Event) error {
	if c.db == nil {
		return errNoDB
	}
	env, subject, err := c.buildEnvelope(ctx, evt)
	if err != nil {
		return err
	}

	envJSON, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("eventbus: marshal envelope: %w", err)
	}
	headers, err := json.Marshal(traceHeaders(ctx))
	if err != nil {
		return fmt.Errorf("eventbus: marshal headers: %w", err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO outbox (id, tenant_id, subject, envelope, headers, created_at, next_attempt_at)
		VALUES ($1, $2, $3, $4, $5, NOW(), NOW())
	`, env.EventID, env.TenantID, subject, envJSON, headers)
	if err != nil {
		return fmt.Errorf("eventbus: insert outbox: %w", err)
	}

	telemetry.LoggerFromContext(ctx).Debug("event enqueued to outbox",
		"event_id", env.EventID,
		"event_type", env.EventType,
		"subject", subject,
	)

	return nil
}

// PublishDirect bypasses the outbox and publishes directly to NATS.
// Reserved for truly stateless events emitted outside any DB transaction
// (CLI tools, startup announcements). Prefer Publish for everything else.
func (c *Client) PublishDirect(ctx context.Context, evt Event) error {
	env, subject, err := c.buildEnvelope(ctx, evt)
	if err != nil {
		return err
	}

	envJSON, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("eventbus: marshal envelope: %w", err)
	}

	ctx, span := telemetry.StartSpan(ctx, "eventbus.publish.direct",
		attribute.String("event.type", env.EventType),
		attribute.String("event.id", env.EventID),
		attribute.String("event.subject", subject),
	)
	defer span.End()

	start := time.Now()
	if _, err := c.js.PublishMsg(ctx, newMsg(subject, envJSON, traceHeaders(ctx))); err != nil {
		telemetry.RecordSpanError(span, err)
		return fmt.Errorf("eventbus: nats publish: %w", err)
	}
	c.publishDuration().Record(ctx, time.Since(start).Seconds())
	c.publishCounter().Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("subject", subject),
			attribute.String("outcome", "ok"),
		))

	telemetry.LoggerFromContext(ctx).Debug("event published direct",
		"event_id", env.EventID,
		"event_type", env.EventType,
	)

	return nil
}

func (c *Client) buildEnvelope(ctx context.Context, evt Event) (*Envelope, string, error) {
	if evt.EventType == "" {
		return nil, "", fmt.Errorf("eventbus: Event.EventType required")
	}
	if evt.TenantID == "" {
		return nil, "", fmt.Errorf("eventbus: Event.TenantID required")
	}
	if evt.Actor.Type == "" || evt.Actor.ID == "" {
		return nil, "", fmt.Errorf("eventbus: Event.Actor.Type and ID required")
	}
	if evt.Payload == nil {
		return nil, "", fmt.Errorf("eventbus: Event.Payload required")
	}

	subject, err := SubjectForEvent(evt.TenantID, evt.EventType)
	if err != nil {
		return nil, "", err
	}

	version, err := versionFromEventType(evt.EventType)
	if err != nil {
		return nil, "", err
	}

	payloadJSON, err := json.Marshal(evt.Payload)
	if err != nil {
		return nil, "", fmt.Errorf("eventbus: marshal payload: %w", err)
	}

	env := &Envelope{
		EventID:       newEventID(),
		EventType:     evt.EventType,
		EventTime:     time.Now().UTC().Truncate(time.Millisecond),
		TenantID:      evt.TenantID,
		Actor:         evt.Actor,
		CorrelationID: correlationID(ctx, evt.CorrelationID),
		CausationID:   evt.CausationID,
		SourceService: c.cfg.ServiceName,
		SchemaVersion: version,
		Payload:       payloadJSON,
	}
	if env.CausationID == "" {
		env.CausationID = consumedEventID(ctx)
	}

	if err := c.validator.ValidateEnvelope(ctx, env); err != nil {
		return nil, "", fmt.Errorf("eventbus: envelope validation: %w", err)
	}
	if err := c.validator.ValidatePayload(ctx, env); err != nil {
		return nil, "", fmt.Errorf("eventbus: payload validation: %w", err)
	}

	return env, subject, nil
}

// correlationID prefers the explicit value, then the request or consumed
// event's correlation ID on ctx, then a new one. The envelope schema
// requires a UUID.
func correlationID(ctx context.Context, explicit string) string {
	for _, id := range []string{explicit, authctx.CorrelationID(ctx)} {
		if _, err := uuid.Parse(id); err == nil {
			return id
		}
	}
	return newEventID()
}

func newEventID() string {
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	return uuid.NewString()
}

// versionFromEventType returns N from an event type ending in ".v<N>".
func versionFromEventType(eventType string) (int, error) {
	i := strings.LastIndex(eventType, ".v")
	if i < 0 {
		return 0, fmt.Errorf("eventbus: no version in %q", eventType)
	}
	n, err := strconv.Atoi(eventType[i+2:])
	if err != nil || n < 1 {
		return 0, fmt.Errorf("eventbus: invalid version suffix in %q", eventType)
	}
	return n, nil
}

// traceHeaders captures the W3C trace context of ctx.
func traceHeaders(ctx context.Context) map[string]string {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return carrier
}

func newMsg(subject string, data []byte, headers map[string]string) *nats.Msg {
	msg := nats.NewMsg(subject)
	msg.Data = data
	for k, v := range headers {
		msg.Header.Set(k, v)
	}
	return msg
}
