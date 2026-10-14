// Package repository is yc-monitor's data access. Callers own the
// transaction and its RLS context (dbkit).
package repository

import (
	"context"

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

// Record inserts d; a redelivered dead letter is ignored.
func (DLQ) Record(ctx context.Context, tx pgx.Tx, d domain.DLQEntry) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO dlq_entries
			(dlq_sequence, tenant_id, service, consumer, event_type, event_id, failure_reason,
			 attempts, last_error, first_failure_at, last_failure_at, envelope)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, ''), $10, $11, $12)
		ON CONFLICT (dlq_sequence) DO NOTHING`,
		d.DLQSequence, d.TenantID, d.Service, d.Consumer, d.EventType, d.EventID, d.FailureReason,
		d.Attempts, d.LastError, d.FirstFailureAt, d.LastFailureAt, d.Envelope)
	return err
}

// List returns the newest dead letters first.
func (DLQ) List(ctx context.Context, tx pgx.Tx, f domain.Filter) ([]domain.DLQEntry, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, dlq_sequence, tenant_id, service, consumer, event_type, event_id, failure_reason,
		       attempts, COALESCE(last_error, ''), first_failure_at, last_failure_at, envelope, created_at
		FROM dlq_entries
		WHERE ($1 = '' OR tenant_id = $1)
		ORDER BY dlq_sequence DESC
		LIMIT $2`, f.TenantID, f.Limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.DLQEntry, error) {
		var d domain.DLQEntry
		err := r.Scan(&d.ID, &d.DLQSequence, &d.TenantID, &d.Service, &d.Consumer, &d.EventType,
			&d.EventID, &d.FailureReason, &d.Attempts, &d.LastError, &d.FirstFailureAt,
			&d.LastFailureAt, &d.Envelope, &d.CreatedAt)
		return d, err
	})
}
