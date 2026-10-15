// Package service holds yc-inventory's use cases. Every mutation writes
// its tables and publishes its events in one transaction.
package service

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	"github.com/slaghuis/yc-platform/services/yc-inventory/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-inventory/internal/repository"
)

// ServiceName is the actor ID of events yc-inventory publishes on its own
// behalf.
const ServiceName = "yc-inventory"

// Publisher publishes through the outbox.
type Publisher interface {
	Publish(ctx context.Context, tx pgx.Tx, evt eventbus.Event) error
}

// Service implements the inventory use cases.
type Service struct {
	db       *pgxpool.Pool
	bus      Publisher
	repo     repository.Stock
	defaults domain.Config
	now      func() time.Time
}

// New returns a Service. defaults applies to shops without settings of
// their own.
func New(db *pgxpool.Pool, bus Publisher, defaults domain.Config) *Service {
	return &Service{db: db, bus: bus, defaults: defaults, now: time.Now}
}

// config returns tenantID's settings or the defaults.
func (s *Service) config(ctx context.Context, tx pgx.Tx, tenantID string) (domain.Config, error) {
	c, ok, err := s.repo.Config(ctx, tx, tenantID)
	if err != nil || ok {
		return c, err
	}
	return s.defaults, nil
}

// InitShop gives a provisioned shop the default settings.
func (s *Service) InitShop(ctx context.Context, tenantID string) error {
	if err := validateShopTenant(tenantID); err != nil {
		return err
	}
	return dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		return s.repo.PutConfig(ctx, tx, tenantID, s.defaults, s.now(), true)
	})
}

// Config returns the calling shop's settings.
func (s *Service) Config(ctx context.Context) (domain.Config, error) {
	var c domain.Config
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		c, err = s.config(ctx, tx, tenantID)
		return err
	})
	return c, err
}

// PutConfig replaces the calling shop's settings. Turning inventory off
// makes every SKU purchasable, so out-of-stock SKUs are announced as
// restored; turning it on announces them as depleted again.
func (s *Service) PutConfig(ctx context.Context, c domain.Config) (domain.Config, error) {
	if err := c.Validate(); err != nil {
		return domain.Config{}, err
	}
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		old, err := s.config(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		now := s.now()
		if err := s.repo.PutConfig(ctx, tx, tenantID, c, now, false); err != nil {
			return err
		}
		c.Stored, c.UpdatedAt = true, now
		if old.Enabled == c.Enabled {
			return nil
		}
		signal := domain.SignalRestored
		if c.Enabled {
			signal = domain.SignalDepleted
		}
		after := ""
		for {
			levels, err := s.repo.ListLevels(ctx, tx, tenantID, repository.LevelFilter{OutOfStock: true}, after, 500)
			if err != nil {
				return err
			}
			for _, l := range levels {
				if err := s.publishSignal(ctx, tx, tenantID, signal, l, now); err != nil {
					return err
				}
			}
			if len(levels) < 500 {
				return nil
			}
			after = levels[len(levels)-1].SKUID
		}
	})
	return c, err
}

// ListStock returns a page of the calling shop's stock levels.
func (s *Service) ListStock(ctx context.Context, f repository.LevelFilter, after string, limit int) ([]domain.Level, string, error) {
	var levels []domain.Level
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		levels, err = s.repo.ListLevels(ctx, tx, tenantID, f, after, limit+1)
		return err
	})
	next := ""
	if len(levels) > limit {
		levels = levels[:limit]
		next = levels[limit-1].SKUID
	}
	return levels, next, err
}

// Stock returns one SKU's level.
func (s *Service) Stock(ctx context.Context, skuID string) (domain.Level, error) {
	var l domain.Level
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		l, err = s.level(ctx, tx, tenantID, skuID, false)
		return err
	})
	return l, err
}

func (s *Service) level(ctx context.Context, tx pgx.Tx, tenantID, skuID string, forUpdate bool) (domain.Level, error) {
	l, ok, err := s.repo.Level(ctx, tx, tenantID, skuID, forUpdate)
	if err != nil {
		return domain.Level{}, err
	}
	if !ok {
		return domain.Level{}, ycerrors.NotFound("tracked SKU", skuID)
	}
	return l, nil
}

// Receive adds received stock to a SKU.
func (s *Service) Receive(ctx context.Context, skuID string, quantity int64, reason string) (domain.Level, error) {
	return s.changeLevel(ctx, skuID, func(l domain.Level, now time.Time) (domain.Level, any, domain.Movement, error) {
		after, err := l.Receive(quantity)
		evt := events.InventoryStockReceivedV1{
			SKUID: skuID, Quantity: quantity, OnHand: after.OnHand, Reserved: after.Reserved, Available: after.Available(),
			ReceivedAt: now.UTC(),
		}
		if reason != "" {
			evt.Reason = &reason
		}
		return after, evt, domain.Movement{Type: domain.MovementReceive, OnHandDelta: quantity, Reason: reason}, err
	})
}

// Adjust corrects a SKU's on-hand stock by delta (shrinkage, counts).
func (s *Service) Adjust(ctx context.Context, skuID string, delta int64, reason string) (domain.Level, error) {
	if reason == "" {
		return domain.Level{}, ycerrors.Validation().Field("reason", "required", "Say why stock is adjusted").Build()
	}
	return s.changeLevel(ctx, skuID, func(l domain.Level, now time.Time) (domain.Level, any, domain.Movement, error) {
		after, err := l.Adjust(delta)
		evt := events.InventoryStockAdjustedV1{
			SKUID: skuID, Delta: delta, OnHand: after.OnHand, Reserved: after.Reserved, Available: after.Available(),
			Reason: reason, AdjustedAt: now.UTC(),
		}
		return after, evt, domain.Movement{Type: domain.MovementAdjust, OnHandDelta: delta, Reason: reason}, err
	})
}

// SetThreshold sets a SKU's low-stock threshold.
func (s *Service) SetThreshold(ctx context.Context, skuID string, threshold int64) (domain.Level, error) {
	return s.changeLevel(ctx, skuID, func(l domain.Level, _ time.Time) (domain.Level, any, domain.Movement, error) {
		after, err := l.SetThreshold(threshold)
		return after, nil, domain.Movement{}, err
	})
}

// changeLevel applies an admin change to one SKU, recording the movement
// (if any) and publishing its event and signals.
func (s *Service) changeLevel(ctx context.Context, skuID string,
	change func(domain.Level, time.Time) (domain.Level, any, domain.Movement, error),
) (domain.Level, error) {
	var after domain.Level
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		before, err := s.level(ctx, tx, tenantID, skuID, true)
		if err != nil {
			return err
		}
		now := s.now()
		var (
			evt any
			m   domain.Movement
		)
		after, evt, m, err = change(before, now)
		if err != nil {
			return err
		}
		if err := s.repo.UpdateLevel(ctx, tx, tenantID, after, now); err != nil {
			return err
		}
		after.UpdatedAt = now
		if m.Type != "" {
			a, _ := authctx.FromContext(ctx)
			m.SKUID, m.ActorID, m.CreatedAt = skuID, a.ActorID, now
			if err := s.repo.AddMovement(ctx, tx, tenantID, m); err != nil {
				return err
			}
		}
		if e, ok := evt.(interface{ EventType() string }); ok {
			if err := s.publish(ctx, tx, tenantID, e.EventType(), e); err != nil {
				return err
			}
		}
		return s.publishSignals(ctx, tx, tenantID, before, after, now)
	})
	return after, err
}

// History returns a page of a SKU's movements, newest first.
func (s *Service) History(ctx context.Context, skuID string, after *repository.MovementCursor, limit int) ([]domain.Movement, *repository.MovementCursor, error) {
	var ms []domain.Movement
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		if _, err := s.level(ctx, tx, tenantID, skuID, false); err != nil {
			return err
		}
		var err error
		ms, err = s.repo.Movements(ctx, tx, tenantID, skuID, after, limit+1)
		return err
	})
	if err != nil || len(ms) <= limit {
		return ms, nil, err
	}
	ms = ms[:limit]
	last := ms[limit-1]
	return ms, &repository.MovementCursor{At: last.CreatedAt, ID: last.ID}, nil
}

// SKUReservations returns a SKU's active reservations.
func (s *Service) SKUReservations(ctx context.Context, skuID string) ([]domain.Reservation, error) {
	var rs []domain.Reservation
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		if _, err := s.level(ctx, tx, tenantID, skuID, false); err != nil {
			return err
		}
		var err error
		rs, err = s.repo.ActiveReservations(ctx, tx, tenantID, skuID)
		return err
	})
	return rs, err
}

// publishSignals publishes depleted, restored and low-stock events for a
// change, unless the shop has inventory turned off.
func (s *Service) publishSignals(ctx context.Context, tx pgx.Tx, tenantID string, before, after domain.Level, now time.Time) error {
	signals := domain.Signals(before, after)
	if len(signals) == 0 {
		return nil
	}
	cfg, err := s.config(ctx, tx, tenantID)
	if err != nil || !cfg.Enabled {
		return err
	}
	for _, sig := range signals {
		if err := s.publishSignal(ctx, tx, tenantID, sig, after, now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) publishSignal(ctx context.Context, tx pgx.Tx, tenantID string, sig domain.Signal, l domain.Level, now time.Time) error {
	at := now.UTC()
	switch sig {
	case domain.SignalDepleted:
		return s.publish(ctx, tx, tenantID, events.TypeInventoryStockDepletedV1, events.InventoryStockDepletedV1{
			SKUID: l.SKUID, OnHand: l.OnHand, Reserved: l.Reserved, Available: l.Available(), DepletedAt: at,
		})
	case domain.SignalRestored:
		return s.publish(ctx, tx, tenantID, events.TypeInventoryStockRestoredV1, events.InventoryStockRestoredV1{
			SKUID: l.SKUID, OnHand: l.OnHand, Reserved: l.Reserved, Available: l.Available(), RestoredAt: at,
		})
	case domain.SignalLow:
		return s.publish(ctx, tx, tenantID, events.TypeInventoryStockLowThresholdReachedV1, events.InventoryStockLowThresholdReachedV1{
			SKUID: l.SKUID, OnHand: l.OnHand, Reserved: l.Reserved, Available: l.Available(), LowThreshold: l.LowThreshold,
			ReachedAt: at,
		})
	}
	return nil
}

// asShopAdmin runs fn in the calling shop admin's shop.
func (s *Service) asShopAdmin(ctx context.Context, fn func(tx pgx.Tx, tenantID string) error) error {
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorShopAdmin {
		return ycerrors.Forbidden("Shop admins only").WithInternal("actor_type %s", a.ActorType)
	}
	if err := validateShopTenant(a.TenantID); err != nil {
		return err
	}
	return dbkit.WithTenantTx(ctx, s.db, a.TenantID, func(tx pgx.Tx) error { return fn(tx, a.TenantID) })
}

// asService runs fn in the calling service's shop (internal routes).
func (s *Service) asService(ctx context.Context, fn func(tx pgx.Tx, tenantID string) error) error {
	a, _ := authctx.FromContext(ctx)
	if err := validateShopTenant(a.TenantID); err != nil {
		return err
	}
	return dbkit.WithTenantTx(ctx, s.db, a.TenantID, func(tx pgx.Tx) error { return fn(tx, a.TenantID) })
}

// publish attributes the event to the caller, or to yc-inventory itself
// when there is none (event handlers, the sweeper).
func (s *Service) publish(ctx context.Context, tx pgx.Tx, tenantID, eventType string, payload any) error {
	actor := eventbus.Actor{Type: string(authctx.ActorService), ID: ServiceName}
	if a, ok := authctx.FromContext(ctx); ok && a.ActorType != "" {
		actor = eventbus.Actor{Type: string(a.ActorType), ID: a.ActorID, AccessTokenJTI: a.AccessTokenJTI}
	}
	return s.bus.Publish(ctx, tx, eventbus.Event{EventType: eventType, TenantID: tenantID, Actor: actor, Payload: payload})
}

func validateShopTenant(tenantID string) error {
	switch tenantID {
	case "":
		return ycerrors.Validation().Field("tenant_id", "required", "Tenant is required").Build()
	case eventbus.PlatformTenantID:
		return ycerrors.Validation().Field("tenant_id", "invalid", "Stock belongs to a shop, not "+tenantID).Build()
	}
	if err := eventbus.ValidateTenantID(tenantID); err != nil {
		return ycerrors.Validation().Field("tenant_id", "invalid", "Tenant ID is not valid").Wrap(err)
	}
	return nil
}
