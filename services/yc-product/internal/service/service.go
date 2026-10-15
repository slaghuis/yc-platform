// Package service holds yc-product's use cases. Every mutation writes the
// catalog, refreshes the search projection and publishes its event in one
// transaction.
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
	"github.com/slaghuis/yc-platform/services/yc-product/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-product/internal/repository"
)

// Publisher publishes through the outbox.
type Publisher interface {
	Publish(ctx context.Context, tx pgx.Tx, evt eventbus.Event) error
}

// Service implements the catalog use cases.
type Service struct {
	db  *pgxpool.Pool
	bus Publisher
	cat repository.Catalog
	now func() time.Time
}

// New returns a Service.
func New(db *pgxpool.Pool, bus Publisher) *Service {
	return &Service{db: db, bus: bus, now: time.Now}
}

// CreateSupplier creates a supplier for tenantID (system admins only).
func (s *Service) CreateSupplier(ctx context.Context, tenantID string, in domain.SupplierInput) (domain.Supplier, error) {
	if err := requireSystemAdmin(ctx); err != nil {
		return domain.Supplier{}, err
	}
	if err := validateShopTenant(tenantID); err != nil {
		return domain.Supplier{}, err
	}
	if err := in.Validate(); err != nil {
		return domain.Supplier{}, err
	}
	var sup domain.Supplier
	err := dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		var err error
		if sup, err = s.cat.CreateSupplier(ctx, tx, tenantID, in); err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypeProductSupplierCreatedV1, events.ProductSupplierCreatedV1{
			SupplierID: sup.ID, Name: sup.Name, Status: string(sup.Status), CreatedAt: sup.CreatedAt.UTC(),
		})
	})
	return sup, err
}

// ListSuppliers lists suppliers of tenantID, or of every shop when "".
func (s *Service) ListSuppliers(ctx context.Context, tenantID string, after *repository.Cursor, limit int) ([]domain.Supplier, *repository.Cursor, error) {
	if err := requireSystemAdmin(ctx); err != nil {
		return nil, nil, err
	}
	var out []domain.Supplier
	err := dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		out, err = s.cat.ListSuppliers(ctx, tx, tenantID, after, limit+1)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	var next *repository.Cursor
	if len(out) > limit {
		out = out[:limit]
		last := out[limit-1]
		next = &repository.Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return out, next, nil
}

// GetSupplier returns any shop's supplier (system admins only).
func (s *Service) GetSupplier(ctx context.Context, id string) (domain.Supplier, error) {
	if err := requireSystemAdmin(ctx); err != nil {
		return domain.Supplier{}, err
	}
	var sup domain.Supplier
	err := dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		sup, err = s.cat.GetSupplier(ctx, tx, id, false)
		return err
	})
	return sup, err
}

// UpdateSupplier replaces a supplier's fields in its own shop.
func (s *Service) UpdateSupplier(ctx context.Context, id string, in domain.SupplierInput) (domain.Supplier, error) {
	if err := in.Validate(); err != nil {
		return domain.Supplier{}, err
	}
	found, err := s.GetSupplier(ctx, id)
	if err != nil {
		return domain.Supplier{}, err
	}
	var sup domain.Supplier
	err = dbkit.WithTenantTx(ctx, s.db, found.TenantID, func(tx pgx.Tx) error {
		var err error
		if sup, err = s.cat.UpdateSupplier(ctx, tx, id, in); err != nil {
			return err
		}
		return s.publish(ctx, tx, sup.TenantID, events.TypeProductSupplierUpdatedV1, events.ProductSupplierUpdatedV1{
			SupplierID: sup.ID, Name: sup.Name, Status: string(sup.Status), UpdatedAt: sup.UpdatedAt.UTC(),
		})
	})
	return sup, err
}

// DeleteSupplier deletes a supplier no product references.
func (s *Service) DeleteSupplier(ctx context.Context, id string) error {
	found, err := s.GetSupplier(ctx, id)
	if err != nil {
		return err
	}
	return dbkit.WithTenantTx(ctx, s.db, found.TenantID, func(tx pgx.Tx) error {
		if err := s.cat.DeleteSupplier(ctx, tx, id); err != nil {
			return err
		}
		return s.publish(ctx, tx, found.TenantID, events.TypeProductSupplierDeletedV1, events.ProductSupplierDeletedV1{
			SupplierID: id, DeletedAt: s.now().UTC(),
		})
	})
}

// PublicSuppliers returns the shop's active suppliers.
func (s *Service) PublicSuppliers(ctx context.Context) ([]domain.Supplier, error) {
	var out []domain.Supplier
	err := s.read(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = s.cat.ActiveSuppliers(ctx, tx)
		return err
	})
	return out, err
}

// read runs fn in the caller's tenant. System admins on _platform read
// every shop.
func (s *Service) read(ctx context.Context, fn dbkit.TxFunc) error {
	a, _ := authctx.FromContext(ctx)
	switch a.TenantID {
	case "":
		return ycerrors.Validation().Field("tenant_id", "required", "Tenant is required").Build()
	case dbkit.PlatformTenantID:
		return dbkit.WithPlatformTx(ctx, s.db, fn)
	default:
		return dbkit.WithTenantTx(ctx, s.db, a.TenantID, fn)
	}
}

// inShop runs fn in the caller's shop for /admin/ mutations. System admins
// use /sysadmin/ and name the shop instead.
func (s *Service) inShop(ctx context.Context, fn func(tx pgx.Tx, tenantID string) error) error {
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorShopAdmin {
		return ycerrors.Forbidden("Shop admins only").WithInternal("actor_type %s", a.ActorType)
	}
	if err := validateShopTenant(a.TenantID); err != nil {
		return err
	}
	return dbkit.WithTenantTx(ctx, s.db, a.TenantID, func(tx pgx.Tx) error { return fn(tx, a.TenantID) })
}

func (s *Service) publish(ctx context.Context, tx pgx.Tx, tenantID, eventType string, payload any) error {
	a, _ := authctx.FromContext(ctx)
	return s.bus.Publish(ctx, tx, eventbus.Event{
		EventType: eventType,
		TenantID:  tenantID,
		Actor:     eventbus.Actor{Type: string(a.ActorType), ID: a.ActorID, AccessTokenJTI: a.AccessTokenJTI},
		Payload:   payload,
	})
}

func requireSystemAdmin(ctx context.Context) error {
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorSystemAdmin {
		return ycerrors.Forbidden("System admins only").WithInternal("actor_type %s", a.ActorType)
	}
	return nil
}

func validateShopTenant(tenantID string) error {
	switch tenantID {
	case "":
		return ycerrors.Validation().Field("tenant_id", "required", "Tenant is required").Build()
	case eventbus.PlatformTenantID:
		return ycerrors.Validation().Field("tenant_id", "invalid", "Name a shop, not "+tenantID).Build()
	}
	if err := eventbus.ValidateTenantID(tenantID); err != nil {
		return ycerrors.Validation().Field("tenant_id", "invalid", "Tenant ID is not valid").Wrap(err)
	}
	return nil
}

func isAdmin(ctx context.Context) bool {
	a, _ := authctx.FromContext(ctx)
	return a.ActorType == authctx.ActorShopAdmin || a.ActorType == authctx.ActorSystemAdmin
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
