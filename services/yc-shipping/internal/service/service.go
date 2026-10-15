// Package service holds yc-shipping's use cases. Every mutation writes its
// tables and publishes its events in one transaction.
package service

import (
	"context"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	"github.com/slaghuis/yc-platform/services/yc-shipping/internal/carrier"
	"github.com/slaghuis/yc-platform/services/yc-shipping/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-shipping/internal/repository"
)

// ServiceName is the actor ID of events yc-shipping publishes on its own
// behalf.
const ServiceName = "yc-shipping"

// Publisher publishes through the outbox.
type Publisher interface {
	Publish(ctx context.Context, tx pgx.Tx, evt eventbus.Event) error
}

// Service implements the shipping use cases.
type Service struct {
	db      *pgxpool.Pool
	bus     Publisher
	drivers map[string]carrier.Driver
	repo    repository.Shipping
	now     func() time.Time
}

// New returns a Service with the given carrier drivers.
func New(db *pgxpool.Pool, bus Publisher, drivers ...carrier.Driver) *Service {
	s := &Service{db: db, bus: bus, drivers: map[string]carrier.Driver{}, now: time.Now}
	for _, d := range drivers {
		s.drivers[d.Name()] = d
	}
	return s
}

// driverNames lists the carrier drivers, sorted.
func (s *Service) driverNames() []string {
	names := make([]string, 0, len(s.drivers))
	for n := range s.drivers {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// Zones returns the calling shop admin's zones in matching order.
func (s *Service) Zones(ctx context.Context) ([]domain.Zone, error) {
	var zones []domain.Zone
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		zones, err = s.repo.Zones(ctx, tx, tenantID)
		return err
	})
	return zones, err
}

// PublicZones returns the shop's zones for its storefront.
func (s *Service) PublicZones(ctx context.Context) ([]domain.Zone, error) {
	var zones []domain.Zone
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		zones, err = s.repo.Zones(ctx, tx, tenantID)
		return err
	})
	return zones, err
}

// CreateZone adds a zone.
func (s *Service) CreateZone(ctx context.Context, z domain.Zone) (domain.Zone, error) {
	z.Normalise()
	if err := z.Validate(); err != nil {
		return domain.Zone{}, err
	}
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		now := s.now().UTC()
		z.ID, z.CreatedAt, z.UpdatedAt = uuid.NewString(), now, now
		return s.repo.PutZone(ctx, tx, tenantID, z)
	})
	return z, err
}

// UpdateZone replaces a zone's name, rules and priority.
func (s *Service) UpdateZone(ctx context.Context, id string, z domain.Zone) (domain.Zone, error) {
	z.Normalise()
	if err := z.Validate(); err != nil {
		return domain.Zone{}, err
	}
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		old, ok, err := s.repo.Zone(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if !ok {
			return ycerrors.NotFound("zone", id)
		}
		z.ID, z.CreatedAt, z.UpdatedAt = id, old.CreatedAt, s.now().UTC()
		return s.repo.PutZone(ctx, tx, tenantID, z)
	})
	return z, err
}

// DeleteZone deletes a zone and its rates.
func (s *Service) DeleteZone(ctx context.Context, id string) error {
	return s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		ok, err := s.repo.DeleteZone(ctx, tx, tenantID, id)
		if err == nil && !ok {
			err = ycerrors.NotFound("zone", id)
		}
		return err
	})
}

// Rates returns the shop's rates, of one zone when zoneID is set.
func (s *Service) Rates(ctx context.Context, zoneID string) ([]domain.Rate, error) {
	var rates []domain.Rate
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		rates, err = s.repo.Rates(ctx, tx, tenantID, zoneID)
		return err
	})
	return rates, err
}

// CreateRate adds a shipping method to a zone.
func (s *Service) CreateRate(ctx context.Context, r domain.Rate) (domain.Rate, error) {
	return s.putRate(ctx, "", r)
}

// UpdateRate replaces a rate.
func (s *Service) UpdateRate(ctx context.Context, id string, r domain.Rate) (domain.Rate, error) {
	return s.putRate(ctx, id, r)
}

func (s *Service) putRate(ctx context.Context, id string, r domain.Rate) (domain.Rate, error) {
	if err := r.Validate(s.driverNames()); err != nil {
		return domain.Rate{}, err
	}
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		if _, ok, err := s.repo.Zone(ctx, tx, tenantID, r.ZoneID); err != nil {
			return err
		} else if !ok {
			return ycerrors.Validation().Field("zone_id", "invalid", "No such zone").Build()
		}
		now := s.now().UTC()
		r.ID, r.CreatedAt, r.UpdatedAt = uuid.NewString(), now, now
		if id != "" {
			old, ok, err := s.repo.Rate(ctx, tx, tenantID, id)
			if err != nil {
				return err
			}
			if !ok {
				return ycerrors.NotFound("rate", id)
			}
			r.ID, r.CreatedAt = id, old.CreatedAt
		}
		err := s.repo.PutRate(ctx, tx, tenantID, r)
		if dbkit.IsUniqueViolation(err) {
			return ycerrors.Conflict("The zone already has a " + r.Method + " rate")
		}
		return err
	})
	return r, err
}

// DeleteRate deletes a rate.
func (s *Service) DeleteRate(ctx context.Context, id string) error {
	return s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		ok, err := s.repo.DeleteRate(ctx, tx, tenantID, id)
		if err == nil && !ok {
			err = ycerrors.NotFound("rate", id)
		}
		return err
	})
}

// asShopAdmin runs fn in the calling shop admin's shop.
func (s *Service) asShopAdmin(ctx context.Context, fn func(tx pgx.Tx, tenantID string) error) error {
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorShopAdmin {
		return ycerrors.Forbidden("Shop admins only").WithInternal("actor_type %s", a.ActorType)
	}
	return s.inShop(ctx, fn)
}

// inShop runs fn in the caller's shop. Routes decide who may call.
func (s *Service) inShop(ctx context.Context, fn func(tx pgx.Tx, tenantID string) error) error {
	a, _ := authctx.FromContext(ctx)
	return s.inTenant(ctx, a.TenantID, fn)
}

func (s *Service) inTenant(ctx context.Context, tenantID string, fn func(tx pgx.Tx, tenantID string) error) error {
	if err := validateShopTenant(tenantID); err != nil {
		return err
	}
	return dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error { return fn(tx, tenantID) })
}

func actorID(ctx context.Context) string {
	a, _ := authctx.FromContext(ctx)
	return a.ActorID
}

// publish attributes the event to the caller, or to yc-shipping itself
// when there is none (event handlers).
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
		return ycerrors.Validation().Field("tenant_id", "invalid", "Shipping belongs to a shop, not "+tenantID).Build()
	}
	if err := eventbus.ValidateTenantID(tenantID); err != nil {
		return ycerrors.Validation().Field("tenant_id", "invalid", "Tenant ID is not valid").Wrap(err)
	}
	return nil
}
