package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
)

// relayAckTimeout bounds the wait for a batch's publish acks; rows still
// unacknowledged are retried.
const relayAckTimeout = 10 * time.Second

// runRelay is the outbox-to-NATS relay goroutine. Each poll drains the
// due rows batch by batch, so a backlog is not limited to one batch per
// interval.
func (c *Client) runRelay() {
	defer c.bgWG.Done()

	ticker := time.NewTicker(c.cfg.OutboxPollInterval)
	defer ticker.Stop()

	logger := telemetry.LoggerFromContext(c.bgCtx)

	for {
		select {
		case <-c.bgCtx.Done():
			return
		case <-ticker.C:
		}
		for c.bgCtx.Err() == nil {
			n, err := c.processOutboxBatch(c.bgCtx)
			if err != nil {
				logger.Warn("outbox relay batch failed", "err", err)
				break
			}
			if n < c.cfg.OutboxBatchSize {
				break
			}
		}
	}
}

type outboxRow struct {
	ID       string
	TenantID string
	Subject  string
	Envelope []byte
	Headers  map[string]string
	Attempts int
}

// inflight is a row published asynchronously, awaiting its ack.
type inflight struct {
	span  trace.Span
	row   outboxRow
	ack   jetstream.PubAckFuture
	start time.Time
}

// processOutboxBatch claims a batch, publishes every row without waiting,
// then collects the acks and marks the acknowledged rows published in one
// statement. It returns the number of rows claimed.
func (c *Client) processOutboxBatch(ctx context.Context) (int, error) {
	rows, err := c.claimOutboxBatch(ctx, c.cfg.OutboxBatchSize)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}

	sent := make([]inflight, 0, len(rows))
	for _, row := range rows {
		// Continue the trace of the request that wrote the row.
		rctx := otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier(row.Headers))
		rctx, span := telemetry.StartSpan(rctx, "eventbus.publish.outbox",
			attribute.String("event.subject", row.Subject),
			attribute.String("event.id", row.ID),
			attribute.Int("event.attempts", row.Attempts),
		)
		start := time.Now()
		ack, err := c.js.PublishMsgAsync(newMsg(row.Subject, row.Envelope, row.Headers))
		if err != nil {
			c.publishFailed(rctx, span, row, err)
			continue
		}
		sent = append(sent, inflight{span: span, row: row, ack: ack, start: start})
	}

	timeout := time.NewTimer(relayAckTimeout)
	defer timeout.Stop()
	published := make([]string, 0, len(sent))
	for i, p := range sent {
		pctx := trace.ContextWithSpan(ctx, p.span)
		select {
		case <-p.ack.Ok():
			c.publishDuration().Record(pctx, time.Since(p.start).Seconds(),
				metric.WithAttributes(attribute.String("subject", p.row.Subject)))
			c.publishCounter().Add(pctx, 1, metric.WithAttributes(
				attribute.String("subject", p.row.Subject),
				attribute.String("outcome", "ok"),
			))
			p.span.End()
			published = append(published, p.row.ID)
		case err := <-p.ack.Err():
			c.publishFailed(pctx, p.span, p.row, err)
		case <-timeout.C:
			for _, q := range sent[i:] {
				c.publishFailed(trace.ContextWithSpan(ctx, q.span), q.span, q.row, fmt.Errorf("no publish ack within %s", relayAckTimeout))
			}
			c.markPublished(ctx, published)
			return len(rows), nil
		case <-ctx.Done():
			// The claimed rows become due again when their claim lapses.
			for _, q := range sent[i:] {
				q.span.End()
			}
			c.markPublished(context.WithoutCancel(ctx), published)
			return len(rows), nil
		}
	}
	c.markPublished(ctx, published)
	return len(rows), nil
}

// claimOutboxBatch selects up to limit due rows and advances their
// next_attempt_at to prevent other polls from re-selecting them during
// the attempt. Postgres SKIP LOCKED allows parallel relay instances
// without deadlock (future-proofing; v1 runs one per service).
func (c *Client) claimOutboxBatch(ctx context.Context, limit int) ([]outboxRow, error) {
	// Push next_attempt_at forward so a slow attempt doesn't block the
	// row forever; final success/failure updates it again.
	rows, err := c.db.Query(ctx, `
		UPDATE outbox
		SET next_attempt_at = NOW() + INTERVAL '30 seconds'
		WHERE id IN (
			SELECT id FROM outbox
			WHERE published_at IS NULL
			  AND next_attempt_at <= NOW()
			ORDER BY created_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, tenant_id, subject, envelope, headers, attempts
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("claim outbox: %w", err)
	}
	defer rows.Close()

	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		if err := rows.Scan(&r.ID, &r.TenantID, &r.Subject, &r.Envelope, &r.Headers, &r.Attempts); err != nil {
			return nil, fmt.Errorf("scan outbox: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// publishFailed retries the row later, or dead-letters it once its
// attempts are used up, and ends its span.
func (c *Client) publishFailed(ctx context.Context, span trace.Span, row outboxRow, err error) {
	defer span.End()
	telemetry.RecordSpanError(span, err)

	// A NATS outage is not the event's fault: retry without using up
	// attempts, so a long outage does not dead-letter every pending row.
	if !c.nc.IsConnected() {
		c.scheduleRetry(ctx, row.ID, row.Attempts, err.Error(), c.cfg.OutboxBackoff(0))
		c.publishCounter().Add(ctx, 1,
			metric.WithAttributes(
				attribute.String("subject", row.Subject),
				attribute.String("outcome", "disconnected"),
			))
		return
	}

	nextAttempt := row.Attempts + 1
	if nextAttempt >= c.cfg.OutboxMaxAttempts {
		c.sendOutboxRowToDLQ(ctx, row, err)
		c.publishCounter().Add(ctx, 1,
			metric.WithAttributes(
				attribute.String("subject", row.Subject),
				attribute.String("outcome", "dlq"),
			))
		return
	}

	wait := c.cfg.OutboxBackoff(nextAttempt)
	c.scheduleRetry(ctx, row.ID, nextAttempt, err.Error(), wait)
	c.publishCounter().Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("subject", row.Subject),
			attribute.String("outcome", "retry"),
		))
}

// markPublished marks the rows published. If it fails the rows are
// published again once their claim lapses; consumers deduplicate.
func (c *Client) markPublished(ctx context.Context, ids []string) {
	if len(ids) == 0 {
		return
	}
	_, err := c.db.Exec(ctx, `
		UPDATE outbox SET published_at = NOW(), last_error = NULL
		WHERE id = ANY($1::uuid[])
	`, ids)
	if err != nil {
		telemetry.LoggerFromContext(ctx).Error("mark published",
			"outbox_rows", len(ids), "err", err)
	}
}

func (c *Client) scheduleRetry(ctx context.Context, id string, attempts int, errMsg string, wait time.Duration) {
	_, err := c.db.Exec(ctx, `
		UPDATE outbox
		SET attempts = $1,
		    last_error = $2,
		    next_attempt_at = NOW() + make_interval(secs => $3)
		WHERE id = $4
	`, attempts, errMsg, wait.Seconds(), id)
	if err != nil {
		telemetry.LoggerFromContext(ctx).Error("schedule retry",
			"outbox_id", id, "err", err)
	}
}

func (c *Client) sendOutboxRowToDLQ(ctx context.Context, row outboxRow, publishErr error) {
	logger := telemetry.LoggerFromContext(ctx)

	var env Envelope
	if err := json.Unmarshal(row.Envelope, &env); err != nil {
		logger.Error("outbox DLQ: unmarshal envelope",
			"outbox_id", row.ID, "err", err)
		c.markPublishedWithError(ctx, row.ID, "dlq_unmarshal_failed")
		return
	}

	domain, err := DomainOfEventType(env.EventType)
	if err != nil {
		logger.Error("outbox DLQ: event domain",
			"outbox_id", row.ID, "err", err)
		c.markPublishedWithError(ctx, row.ID, "dlq_domain_failed")
		return
	}
	dlqSubject, err := SubjectForDLQ(env.TenantID, domain)
	if err != nil {
		logger.Error("outbox DLQ: subject",
			"outbox_id", row.ID, "err", err)
		c.markPublishedWithError(ctx, row.ID, "dlq_subject_failed")
		return
	}

	dlqEnv := DLQEnvelope{
		OriginalEvent: env,
		DLQMetadata: DLQMetadata{
			Consumer:      c.cfg.ServiceName + ".outbox-relay",
			Attempts:      row.Attempts + 1,
			FirstFailure:  env.EventTime,
			LastFailure:   time.Now().UTC(),
			LastError:     publishErr.Error(),
			FailureReason: "exhausted",
		},
	}
	payload, err := json.Marshal(dlqEnv)
	if err != nil {
		logger.Error("outbox DLQ: marshal", "outbox_id", row.ID, "err", err)
		return
	}

	if _, err := c.js.Publish(ctx, dlqSubject, payload); err != nil {
		logger.Error("outbox DLQ: publish",
			"outbox_id", row.ID, "subject", dlqSubject, "err", err)
		return
	}

	c.markPublishedWithError(ctx, row.ID, publishErr.Error())
	logger.Warn("event sent to DLQ from outbox",
		"outbox_id", row.ID, "subject", dlqSubject, "err", publishErr)
}

// markPublishedWithError treats the row as "done, do not retry" by setting
// published_at while retaining the last_error for forensic purposes.
func (c *Client) markPublishedWithError(ctx context.Context, id, errMsg string) {
	_, err := c.db.Exec(ctx, `
		UPDATE outbox SET published_at = NOW(), last_error = $1
		WHERE id = $2
	`, errMsg, id)
	if err != nil {
		telemetry.LoggerFromContext(ctx).Error("mark published w/ error",
			"outbox_id", id, "err", err)
	}
}
