// Package service holds yc-monitor's use cases.
package service

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	"github.com/slaghuis/yc-platform/services/yc-monitor/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-monitor/internal/repository"
)

// MaxLimit caps a listing.
const MaxLimit = 500

// Service records events and dead letters and lists them for system admins.
type Service struct {
	db     *pgxpool.Pool
	events repository.Events
	dlq    repository.DLQ
}

// New returns a Service.
func New(db *pgxpool.Pool) *Service {
	return &Service{db: db}
}

// RecordEvent stores a consumed event under its tenant.
func (s *Service) RecordEvent(ctx context.Context, e domain.ReceivedEvent) error {
	return tenantTx(ctx, s.db, e.TenantID, func(tx pgx.Tx) error {
		return s.events.Record(ctx, tx, e)
	})
}

// RecordDeadLetter stores a dead letter under the DLQ subject's tenant.
func (s *Service) RecordDeadLetter(ctx context.Context, d domain.DLQEntry) error {
	return tenantTx(ctx, s.db, d.TenantID, func(tx pgx.Tx) error {
		return s.dlq.Record(ctx, tx, d)
	})
}

// ListEvents returns received events across tenants (system admin).
func (s *Service) ListEvents(ctx context.Context, f domain.Filter) ([]domain.ReceivedEvent, error) {
	var out []domain.ReceivedEvent
	err := dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		out, err = s.events.List(ctx, tx, clamp(f))
		return err
	})
	return out, err
}

// ListDeadLetters returns dead letters across tenants (system admin).
func (s *Service) ListDeadLetters(ctx context.Context, f domain.Filter) ([]domain.DLQEntry, error) {
	var out []domain.DLQEntry
	err := dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		out, err = s.dlq.List(ctx, tx, clamp(f))
		return err
	})
	return out, err
}

// tenantTx writes under tenantID's RLS context. Platform rows are written
// in the platform context, which the policies admit only for _platform rows.
func tenantTx(ctx context.Context, db *pgxpool.Pool, tenantID string, fn dbkit.TxFunc) error {
	if tenantID == dbkit.PlatformTenantID {
		return dbkit.WithPlatformTx(ctx, db, fn)
	}
	return dbkit.WithTenantTx(ctx, db, tenantID, fn)
}

func clamp(f domain.Filter) domain.Filter {
	if f.Limit <= 0 || f.Limit > MaxLimit {
		f.Limit = 100
	}
	return f
}
