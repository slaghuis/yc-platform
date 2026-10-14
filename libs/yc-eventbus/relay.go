package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
)

// runRelay is the outbox-to-NATS relay goroutine. It polls for
// unpublished rows and attempts to publish them.
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
			if err := c.processOutboxBatch(c.bgCtx); err != nil {
				logger.Warn("outbox relay batch failed", "err", err)
			}
		}
	}
}

type outboxRow struct {
	ID       string
	TenantID string
	Subject  string
	Envelope []byte
	Attempts int
}

func (c *Client) processOutboxBatch(ctx context.Context) error {
	rows, err := c.claimOutboxBatch(ctx, c.cfg.OutboxBatchSize)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}

	for _, row := range rows {
		c.processOutboxRow(ctx, row)
	}
	return nil
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
		RETURNING id, tenant_id, subject, envelope, attempts
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("claim outbox: %w", err)
	}
	defer rows.Close()

	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		if err := rows.Scan(&r.ID, &r.TenantID, &r.Subject, &r.Envelope, &r.Attempts); err != nil {
			return nil, fmt.Errorf("scan outbox: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (c *Client) processOutboxRow(ctx context.Context, row outboxRow) {
	ctx, span := telemetry.StartSpan(ctx, "eventbus.publish.outbox",
		attribute.String("event.subject", row.Subject),
		attribute.String("event.id", row.ID),
		attribute.Int("event.attempts", row.Attempts),
	)
	defer span.End()

	start := time.Now()
	_, err := c.js.Publish(ctx, row.Subject, row.Envelope)
	c.publishDuration().Record(ctx, time.Since(start).Seconds(),
		metric.WithAttributes(attribute.String("subject", row.Subject)))

	if err == nil {
		c.markPublished(ctx, row.ID)
		c.publishCounter().Add(ctx, 1,
			metric.WithAttributes(
				attribute.String("subject", row.Subject),
				attribute.String("outcome", "ok"),
			))
		return
	}

	telemetry.RecordSpanError(span, err)

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

func (c *Client) markPublished(ctx context.Context, id string) {
	_, err := c.db.Exec(ctx, `
		UPDATE outbox SET published_at = NOW(), last_error = NULL
		WHERE id = $1
	`, id)
	if err != nil {
		telemetry.LoggerFromContext(ctx).Error("mark published",
			"outbox_id", id, "err", err)
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

// Compile-time check that outboxRow is unused outside this file in a way
// that would require making it exported.
var _ = pgx.ErrNoRows
