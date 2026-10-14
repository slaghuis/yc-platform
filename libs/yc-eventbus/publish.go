package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"

	"github.com/<org>/yc-platform/libs/yc-telemetry"
)

// Publish writes an event to the outbox within the caller's transaction.
// The event is published to NATS by the relay goroutine; this call does
// not touch NATS.
//
// The tx must be live; this method does not commit.
func (c *Client) Publish(ctx context.Context, tx pgx.Tx, evt Event) error {
	env, subject, err := c.buildEnvelope(ctx, evt)
	if err != nil {
		return err
	}

	envJSON, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("eventbus: marshal envelope: %w", err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO outbox (id, tenant_id, subject, envelope, created_at, next_attempt_at)
		VALUES ($1, $2, $3, $4, NOW(), NOW())
	`, env.EventID, env.TenantID, subject, envJSON)
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
// (startup announcements, periodic heartbeats). Prefer Publish for
// everything else.
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
	if _, err := c.js.Publish(ctx, subject, envJSON); err != nil {
		telemetry.RecordSpanError(span, err)
		return fmt.Errorf("eventbus: nats publish: %w", err)
	}
	c.publishDuration().Record(ctx, time.Since(start).Seconds())
	c.publishCounter().Add(ctx, 1, metricAttr("subject", subject))

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

	correlationID := evt.CorrelationID
	if correlationID == "" {
		correlationID = uuid.NewString()
	}

	env := &Envelope{
		EventID:       uuid.NewString(),
		EventType:     evt.EventType,
		EventTime:     time.Now().UTC(),
		TenantID:      evt.TenantID,
		Actor:         evt.Actor,
		CorrelationID: correlationID,
		CausationID:   evt.CausationID,
		SourceService: c.cfg.ServiceName,
		SchemaVersion: version,
		Payload:       payloadJSON,
	}

	if err := c.validator.ValidateEnvelope(ctx, env); err != nil {
		return nil, "", fmt.Errorf("eventbus: envelope validation: %w", err)
	}
	if err := c.validator.ValidatePayload(ctx, env); err != nil {
		return nil, "", fmt.Errorf("eventbus: payload validation: %w", err)
	}

	return env, subject, nil
}

func versionFromEventType(eventType string) (int, error) {
	// eventType validated earlier; last segment is v<N>.
	// Minimal parse that doesn't pull in strconv for int-only.
	last := eventType[len(eventType)-1]
	// Support v1 through v9 cheaply, then fall through to a full parse
	// for v10+. Guard against invalid input.
	n := 0
	for i := len(eventType) - 1; i >= 0 && eventType[i] != 'v'; i-- {
		if eventType[i] < '0' || eventType[i] > '9' {
			return 0, fmt.Errorf("eventbus: invalid version suffix in %q", eventType)
		}
	}
	// Find 'v' position.
	vPos := -1
	for i := len(eventType) - 1; i >= 0; i-- {
		if eventType[i] == 'v' {
			vPos = i
			break
		}
	}
	if vPos < 0 || vPos == len(eventType)-1 {
		return 0, fmt.Errorf("eventbus: no version in %q", eventType)
	}
	for i := vPos + 1; i < len(eventType); i++ {
		n = n*10 + int(eventType[i]-'0')
	}
	_ = last
	return n, nil
}

func metricAttr(k, v string) interface{ Add(...any) } {
	// metric.WithAttributes is used by callers; this helper centralizes
	// construction. Kept simple on purpose; a dedicated helper type would
	// be overkill.
	return nil
}