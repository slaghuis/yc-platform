package service

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	"github.com/slaghuis/yc-platform/services/yc-inventory/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-inventory/internal/repository"
)

// SetTracked follows a SKU's inventory_tracked flag (product.sku.created
// and updated). A newly tracked SKU starts at zero, so it is announced as
// depleted until stock is received. An untracked or deleted SKU has no
// stock row; yc-product treats it as in stock. Active reservations keep
// their lines, and settling them skips the SKU.
func (s *Service) SetTracked(ctx context.Context, tenantID, skuID string, tracked bool) error {
	if err := validateShopTenant(tenantID); err != nil {
		return err
	}
	return dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		if !tracked {
			_, _, err := s.repo.DeleteLevel(ctx, tx, tenantID, skuID)
			return err
		}
		now := s.now()
		l, created, err := s.repo.InsertLevel(ctx, tx, tenantID, skuID, now)
		if err != nil || !created {
			return err
		}
		cfg, err := s.config(ctx, tx, tenantID)
		if err != nil || !cfg.Enabled {
			return err
		}
		return s.publishSignal(ctx, tx, tenantID, domain.SignalDepleted, l, now)
	})
}

// sweepBatch bounds the reservations one sweep expires.
const sweepBatch = 200

// ExpireDue expires active reservations past their TTL, returning how
// many it expired.
func (s *Service) ExpireDue(ctx context.Context) (int, error) {
	var due []repository.DueReservation
	err := dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		due, err = s.repo.DueReservations(ctx, tx, s.now(), sweepBatch)
		return err
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, d := range due {
		ok, err := s.Expire(ctx, d.TenantID, d.ID)
		if err != nil {
			return n, err
		}
		if ok {
			n++
		}
	}
	return n, nil
}

// RunSweeper expires due reservations every interval until ctx ends.
func (s *Service) RunSweeper(ctx context.Context, interval time.Duration) {
	logger := telemetry.LoggerFromContext(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		n, err := s.ExpireDue(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			logger.Warn("reservation sweep failed", "err", err, "expired", n)
		case n > 0:
			logger.Info("reservations expired", "expired", n)
		}
	}
}
