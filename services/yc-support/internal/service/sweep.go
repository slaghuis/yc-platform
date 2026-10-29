package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	"github.com/slaghuis/yc-platform/services/yc-support/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-support/internal/repository"
)

// idleClosedBody is the system message that records an automatic close.
func idleClosedBody(ttl time.Duration) string {
	return fmt.Sprintf("This request was closed automatically after %d hours without a message.", int(ttl.Hours()))
}

// RunSweeper closes idle requests every interval until ctx is done.
func (s *Service) RunSweeper(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := s.SweepIdle(ctx); err != nil && ctx.Err() == nil {
			telemetry.LoggerFromContext(ctx).Error("support: idle sweep", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// SweepIdle closes the open requests, across shops, with no message for
// the idle window, and returns how many it found due. Each close records a
// system message, publishes the events and notifies the requester and the
// shop.
func (s *Service) SweepIdle(ctx context.Context) (int, error) {
	var due []repository.Due
	err := dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		due, err = s.requests.DueIdle(ctx, tx, s.now().UTC().Add(-s.cfg.IdleTTL), s.cfg.BatchSize)
		return err
	})
	if err != nil {
		return 0, err
	}
	var errs []error
	for _, d := range due {
		if ctx.Err() != nil {
			break
		}
		errs = append(errs, s.closeIdle(serviceContext(ctx, d.TenantID), d))
	}
	return len(due), errors.Join(errs...)
}

func (s *Service) closeIdle(ctx context.Context, d repository.Due) error {
	var closed domain.Request
	err := s.inTenant(ctx, d.TenantID, func(tx pgx.Tx, tenantID string) error {
		now := s.now().UTC()
		r, ok, err := s.requests.LockIdle(ctx, tx, tenantID, d.ID, now.Add(-s.cfg.IdleTTL))
		if err != nil || !ok {
			return err
		}
		if err := r.Close(domain.ClosedIdle, now); err != nil {
			return err
		}
		m := domain.Message{
			ID: uuid.NewString(), RequestID: r.ID, AuthorType: domain.AuthorSystem, Body: idleClosedBody(s.cfg.IdleTTL), CreatedAt: now,
		}
		if err := s.messages.Insert(ctx, tx, tenantID, m); err != nil {
			return err
		}
		if err := s.requests.Update(ctx, tx, r); err != nil {
			return err
		}
		if err := s.publishPosted(ctx, tx, r, m); err != nil {
			return err
		}
		if err := s.publishClosed(ctx, tx, r, ""); err != nil {
			return err
		}
		closed = r
		return nil
	})
	if err != nil {
		return err
	}
	if closed.ID != "" {
		s.notifyClosed(ctx, closed)
	}
	return nil
}
