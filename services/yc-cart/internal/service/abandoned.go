package service

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	"github.com/slaghuis/yc-platform/services/yc-cart/internal/repository"
)

// Abandonment configures abandoned-cart detection.
type Abandonment struct {
	// After is how long a customer's cart with lines stays idle before it
	// is announced.
	After time.Duration
	// BatchSize bounds each sweep.
	BatchSize int
}

// SweepAbandoned publishes cart.abandoned.v1 for customers' carts of
// abandonable types that have lines and have been idle for the configured
// time, once per idle spell, and returns how many it announced.
func (s *Service) SweepAbandoned(ctx context.Context) (int, error) {
	var types []string
	for _, name := range s.types.Names() {
		if s.types[name].Abandonable {
			types = append(types, name)
		}
	}
	if len(types) == 0 || s.abandon.After <= 0 {
		return 0, nil
	}
	before := s.now().Add(-s.abandon.After)
	var due []repository.Due
	err := dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		due, err = s.repo.IdleCarts(ctx, tx, types, before, s.abandon.BatchSize)
		return err
	})
	if err != nil {
		return 0, err
	}
	var errs []error
	n := 0
	for _, d := range due {
		if ctx.Err() != nil {
			break
		}
		ok, err := s.announceAbandoned(ctx, d, before)
		if ok {
			n++
		}
		errs = append(errs, err)
	}
	return n, errors.Join(errs...)
}

func (s *Service) announceAbandoned(ctx context.Context, d repository.Due, before time.Time) (bool, error) {
	announced := false
	err := dbkit.WithTenantTx(ctx, s.db, d.TenantID, func(tx pgx.Tx) error {
		cart, ok, err := s.repo.LockIdle(ctx, tx, d.TenantID, d.CartID, before)
		if err != nil || !ok || len(cart.Items) == 0 {
			return err
		}
		now := s.now()
		if err := s.repo.MarkAbandoned(ctx, tx, cart.ID, now); err != nil {
			return err
		}
		quantity := 0
		for _, it := range cart.Items {
			quantity += it.Quantity
		}
		announced = true
		return s.publish(ctx, tx, d.TenantID, events.TypeCartAbandonedV1, events.CartAbandonedV1{
			CartID: cart.ID, UserID: cart.UserID, CartType: cart.Type, ItemCount: int64(len(cart.Items)),
			Quantity: int64(quantity), LastActivityAt: cart.UpdatedAt.UTC(), AbandonedAt: now.UTC(),
		})
	})
	return announced, err
}

// RunAbandonedSweeper sweeps every interval until ctx is done.
func (s *Service) RunAbandonedSweeper(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := s.SweepAbandoned(ctx); err != nil && ctx.Err() == nil {
			telemetry.LoggerFromContext(ctx).Error("abandoned cart sweep", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// AbandonedCarts lists the shop's carts announced as abandoned and not
// used since, most recently abandoned first.
func (s *Service) AbandonedCarts(ctx context.Context, after *repository.AbandonedCursor, limit int) ([]repository.Abandoned, *repository.AbandonedCursor, error) {
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorShopAdmin {
		return nil, nil, ycerrors.Forbidden("Shop admins only").WithInternal("actor_type %s", a.ActorType)
	}
	if err := validateShopTenant(a.TenantID); err != nil {
		return nil, nil, err
	}
	var rows []repository.Abandoned
	err := dbkit.WithTenantTx(ctx, s.db, a.TenantID, func(tx pgx.Tx) error {
		var err error
		rows, err = s.repo.AbandonedCarts(ctx, tx, a.TenantID, after, limit+1)
		return err
	})
	if err != nil || len(rows) <= limit {
		return rows, nil, err
	}
	rows = rows[:limit]
	last := rows[limit-1]
	return rows, &repository.AbandonedCursor{AbandonedAt: last.AbandonedAt, ID: last.CartID}, nil
}
