package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
)

// DeadLetter is a message from a DLQ subject (ADR-003).
type DeadLetter struct {
	DLQEnvelope

	// Subject is the DLQ subject, yc.<tenant_id>.<domain>.dlq.v1.
	Subject string

	// TenantID and Domain are taken from Subject. TenantID is
	// PlatformTenantID for unparseable or tenant-mismatched events.
	TenantID string
	Domain   string

	// Sequence is the DLQ stream sequence. An event can be dead-lettered
	// more than once (across replays), so this, not the original event ID,
	// identifies the dead letter; use it as the idempotency key.
	Sequence uint64

	// Raw is the DLQ message body.
	Raw []byte
}

// DLQHandler processes a dead letter. A nil error acks it; ErrPoison
// discards it at once; any other error retries with backoff up to
// MaxRetries, then discards it.
type DLQHandler func(ctx context.Context, dl DeadLetter) error

// SubscribeDLQ creates a durable consumer on DLQ subjects, e.g.
// "yc.*.*.dlq.v1" for yc-monitor. Unlike Subscribe it never dead-letters
// (that would loop): a dead letter that is malformed or keeps failing is
// logged at error level and discarded by this consumer. It stays in the
// DLQ stream for the replay tooling. There is no processed_events dedup;
// key on DeadLetter.Sequence instead. Needs no database.
func (c *Client) SubscribeDLQ(ctx context.Context, opts SubscribeOpts, h DLQHandler) error {
	opts.applyDefaults()
	if err := opts.validate(); err != nil {
		return err
	}
	if !strings.HasSuffix(opts.Subject, ".dlq.v1") {
		return fmt.Errorf("subscribe: %q is not a DLQ subject (yc.<tenant>.<domain>.dlq.v1)", opts.Subject)
	}
	//nolint:contextcheck // the subscription outlives ctx and stops with the client's background context
	return c.startConsumer(ctx, opts, func(msg jetstream.Msg) { c.handleDeadLetter(opts, h, msg) })
}

func (c *Client) handleDeadLetter(opts SubscribeOpts, h DLQHandler, msg jetstream.Msg) {
	ctx, span := telemetry.StartSpan(extractTrace(c.bgCtx, msg.Headers()), "dlq.consume",
		attribute.String("event.subject", msg.Subject()),
		attribute.String("durable", opts.DurableName),
	)
	defer span.End()

	dl, deliveries, err := parseDeadLetter(msg)
	if err != nil {
		telemetry.RecordSpanError(span, err)
		telemetry.LoggerFromContext(ctx).Error("malformed dead letter discarded",
			"durable", opts.DurableName, "subject", msg.Subject(), "err", err)
		_ = msg.Term()
		c.countDLQOutcome(ctx, opts, "malformed")
		return
	}

	ctx = authctx.WithAuth(ctx, authctx.AuthContext{
		TenantID:      dl.TenantID,
		CorrelationID: dl.OriginalEvent.CorrelationID,
	})
	logger := telemetry.LoggerFromContext(ctx)
	span.SetAttributes(
		attribute.String("event.id", dl.OriginalEvent.EventID),
		attribute.String("event.type", dl.OriginalEvent.EventType),
		attribute.Int64("dlq.sequence", int64(dl.Sequence)), //nolint:gosec // stream sequences fit in int64
	)

	herr := h(ctx, dl)
	switch {
	case herr == nil:
		_ = msg.Ack()
		c.countDLQOutcome(ctx, opts, "ok")
	case IsPoison(herr) || deliveries >= opts.MaxRetries+1:
		telemetry.RecordSpanError(span, herr)
		logger.Error("dead letter discarded by consumer",
			"durable", opts.DurableName, "dlq_sequence", dl.Sequence,
			"event_id", dl.OriginalEvent.EventID, "attempt", deliveries, "err", herr)
		_ = msg.Term()
		c.countDLQOutcome(ctx, opts, "discarded")
	default:
		logger.Warn("dead letter handler failed, will retry",
			"durable", opts.DurableName, "dlq_sequence", dl.Sequence, "attempt", deliveries, "err", herr)
		_ = msg.NakWithDelay(c.cfg.OutboxBackoff(deliveries))
		c.countDLQOutcome(ctx, opts, "retry")
	}
}

func parseDeadLetter(msg jetstream.Msg) (DeadLetter, int, error) {
	meta, err := msg.Metadata()
	if err != nil {
		return DeadLetter{}, 0, fmt.Errorf("metadata: %w", err)
	}
	parts := strings.Split(msg.Subject(), ".")
	if len(parts) != 5 || parts[3] != "dlq" || parts[4] != "v1" {
		return DeadLetter{}, 0, fmt.Errorf("not a DLQ subject")
	}
	tenant, err := TenantOfSubject(msg.Subject())
	if err != nil {
		return DeadLetter{}, 0, err
	}
	dl := DeadLetter{
		Subject:  msg.Subject(),
		TenantID: tenant,
		Domain:   parts[2],
		Sequence: meta.Sequence.Stream,
		Raw:      msg.Data(),
	}
	if err := json.Unmarshal(msg.Data(), &dl.DLQEnvelope); err != nil {
		return DeadLetter{}, 0, fmt.Errorf("unmarshal: %w", err)
	}
	return dl, int(meta.NumDelivered), nil //nolint:gosec // bounded by the consumer's MaxDeliver
}

func (c *Client) countDLQOutcome(ctx context.Context, opts SubscribeOpts, outcome string) {
	c.consumeCounter().Add(ctx, 1, metric.WithAttributes(
		attribute.String("outcome", "dlq_"+outcome),
		attribute.String("durable", opts.DurableName),
	))
}
