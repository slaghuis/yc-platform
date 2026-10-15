// Package repository is yc-monitor's data access. Callers own the
// transaction and its RLS context (dbkit).
package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/slaghuis/yc-platform/services/yc-monitor/internal/domain"
)

// Events stores received events.
type Events struct{}

// Record inserts e; a redelivered event is ignored.
func (Events) Record(ctx context.Context, tx pgx.Tx, e domain.ReceivedEvent) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO received_events
			(event_id, tenant_id, event_type, source_service, correlation_id, event_time, envelope)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $7)
		ON CONFLICT (event_id) DO NOTHING`,
		e.EventID, e.TenantID, e.EventType, e.SourceService, e.CorrelationID, e.EventTime, e.Envelope)
	return err
}

// List returns the newest events first.
func (Events) List(ctx context.Context, tx pgx.Tx, f domain.Filter) ([]domain.ReceivedEvent, error) {
	rows, err := tx.Query(ctx, `
		SELECT event_id, tenant_id, event_type, source_service, COALESCE(correlation_id, ''),
		       event_time, envelope, received_at
		FROM received_events
		WHERE ($1 = '' OR tenant_id = $1)
		ORDER BY received_at DESC, event_id
		LIMIT $2`, f.TenantID, f.Limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.ReceivedEvent, error) {
		var e domain.ReceivedEvent
		err := r.Scan(&e.EventID, &e.TenantID, &e.EventType, &e.SourceService, &e.CorrelationID,
			&e.EventTime, &e.Envelope, &e.ReceivedAt)
		return e, err
	})
}

// DLQ stores dead letters.
type DLQ struct{}

// Record inserts d and reports whether it is new; a redelivered dead
// letter is ignored.
func (DLQ) Record(ctx context.Context, tx pgx.Tx, d domain.DLQEntry) (bool, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO dlq_entries
			(dlq_sequence, tenant_id, service, consumer, event_type, event_id, failure_reason,
			 attempts, last_error, first_failure_at, last_failure_at, envelope)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, ''), $10, $11, $12)
		ON CONFLICT (dlq_sequence) DO NOTHING`,
		d.DLQSequence, d.TenantID, d.Service, d.Consumer, d.EventType, d.EventID, d.FailureReason,
		d.Attempts, d.LastError, d.FirstFailureAt, d.LastFailureAt, d.Envelope)
	if err != nil {
		return false, fmt.Errorf("record dead letter: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// List returns the newest dead letters first.
func (DLQ) List(ctx context.Context, tx pgx.Tx, f domain.Filter) ([]domain.DLQEntry, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+dlqColumns+`
		FROM dlq_entries
		WHERE ($1 = '' OR tenant_id = $1) AND ($3 = '' OR service = $3)
		  AND ($4::boolean IS NULL OR acknowledged = $4)
		ORDER BY dlq_sequence DESC
		LIMIT $2`, f.TenantID, f.Limit, f.Service, f.Acknowledged)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.DLQEntry, error) { return scanDLQ(r) })
}

const dlqColumns = `id, dlq_sequence, tenant_id, service, consumer, event_type, event_id, failure_reason,
	attempts, COALESCE(last_error, ''), first_failure_at, last_failure_at, envelope, created_at,
	acknowledged, COALESCE(acknowledged_by, ''), acknowledged_at`

func scanDLQ(row pgx.Row) (domain.DLQEntry, error) {
	var d domain.DLQEntry
	err := row.Scan(&d.ID, &d.DLQSequence, &d.TenantID, &d.Service, &d.Consumer, &d.EventType,
		&d.EventID, &d.FailureReason, &d.Attempts, &d.LastError, &d.FirstFailureAt,
		&d.LastFailureAt, &d.Envelope, &d.CreatedAt, &d.Acknowledged, &d.AcknowledgedBy, &d.AcknowledgedAt)
	return d, err
}

// TenantOf returns the tenant of the dead letter with that sequence.
func (DLQ) TenantOf(ctx context.Context, tx pgx.Tx, sequence uint64) (string, bool, error) {
	var tenantID string
	err := tx.QueryRow(ctx, `SELECT tenant_id FROM dlq_entries WHERE dlq_sequence = $1`, sequence).Scan(&tenantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("find dead letter: %w", err)
	}
	return tenantID, true, nil
}

// Acknowledge marks the dead letter handled (e.g. replayed) and returns
// it; it reports false when there is none with that sequence.
func (DLQ) Acknowledge(ctx context.Context, tx pgx.Tx, sequence uint64, by string, now time.Time) (domain.DLQEntry, bool, error) {
	d, err := scanDLQ(tx.QueryRow(ctx, `
		UPDATE dlq_entries SET acknowledged = TRUE,
			acknowledged_by = CASE WHEN acknowledged THEN acknowledged_by ELSE $2 END,
			acknowledged_at = CASE WHEN acknowledged THEN acknowledged_at ELSE $3 END
		WHERE dlq_sequence = $1
		RETURNING `+dlqColumns, sequence, by, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return d, false, nil
	}
	if err != nil {
		return d, false, fmt.Errorf("acknowledge dead letter: %w", err)
	}
	return d, true, nil
}

// Summary counts unacknowledged dead letters per tenant and service.
func (DLQ) Summary(ctx context.Context, tx pgx.Tx, tenantID string) ([]domain.DLQSummary, error) {
	rows, err := tx.Query(ctx, `
		SELECT tenant_id, service, COUNT(*), MAX(created_at) FROM dlq_entries
		WHERE NOT acknowledged AND ($1 = '' OR tenant_id = $1)
		GROUP BY tenant_id, service ORDER BY tenant_id, service`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("dead letter summary: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.DLQSummary, error) {
		var s domain.DLQSummary
		err := r.Scan(&s.TenantID, &s.Service, &s.Unacknowledged, &s.Latest)
		return s, err
	})
}

// Purge deletes dead letters and received events created before cutoff.
func (DLQ) Purge(ctx context.Context, tx pgx.Tx, dlqBefore, eventsBefore time.Time) (int64, error) {
	a, err := tx.Exec(ctx, `DELETE FROM dlq_entries WHERE created_at < $1`, dlqBefore)
	if err != nil {
		return 0, fmt.Errorf("purge dead letters: %w", err)
	}
	b, err := tx.Exec(ctx, `DELETE FROM received_events WHERE received_at < $1`, eventsBefore)
	if err != nil {
		return 0, fmt.Errorf("purge received events: %w", err)
	}
	return a.RowsAffected() + b.RowsAffected(), nil
}
