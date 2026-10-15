// Package service holds yc-analytics' use cases: recording facts from
// business events, keeping the aggregates in step with them, announcing
// milestones and answering the dashboards (docs/services/yc-analytics.md,
// ADR-025).
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
	"github.com/slaghuis/yc-platform/services/yc-analytics/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-analytics/internal/repository"
)

// ServiceName is the actor ID of events yc-analytics publishes.
const ServiceName = "yc-analytics"

// Publisher publishes through the outbox.
type Publisher interface {
	Publish(ctx context.Context, tx pgx.Tx, evt eventbus.Event) error
}

// Config tunes the service.
type Config struct {
	Milestones domain.MilestoneRules
	// FunnelCartTypes are the cart types counted as carts in the funnel.
	FunnelCartTypes []string
	// RefreshBatch caps the dirty days rebuilt per refresh.
	RefreshBatch int
	// NightlyDays is how many days, ending yesterday, the nightly
	// rebuild covers.
	NightlyDays int
}

// Service implements the analytics use cases.
type Service struct {
	db      *pgxpool.Pool
	bus     Publisher
	cfg     Config
	now     func() time.Time
	facts   repository.Facts
	aggs    repository.Aggregates
	reports repository.Reports
}

// New returns a Service.
func New(db *pgxpool.Pool, bus Publisher, cfg Config) *Service {
	if cfg.RefreshBatch <= 0 {
		cfg.RefreshBatch = 100
	}
	if cfg.NightlyDays <= 0 {
		cfg.NightlyDays = 2
	}
	return &Service{db: db, bus: bus, cfg: cfg, now: time.Now}
}

func (s *Service) publish(ctx context.Context, tx pgx.Tx, tenantID string, payload interface{ EventType() string }) error {
	return s.bus.Publish(ctx, tx, eventbus.Event{
		EventType: payload.EventType(), TenantID: tenantID,
		Actor:   eventbus.Actor{Type: string(authctx.ActorService), ID: ServiceName},
		Payload: payload,
	})
}

// shopTx writes facts under a shop's RLS context; events under _platform
// are not shop facts.
func (s *Service) shopTx(ctx context.Context, tenantID string, fn dbkit.TxFunc) error {
	if tenantID == "" || tenantID == dbkit.PlatformTenantID {
		return ycerrors.Validation().
			Field("tenant_id", "invalid", "analytics facts belong to a shop, not "+tenantID).Build()
	}
	return dbkit.WithTenantTx(ctx, s.db, tenantID, fn)
}

// read runs fn for a system admin over every shop (tenantID "") or for a
// shop admin over their shop.
func (s *Service) read(ctx context.Context, fn func(tx pgx.Tx, tenantID string) error) error {
	a, _ := authctx.FromContext(ctx)
	switch a.ActorType {
	case authctx.ActorSystemAdmin:
		return dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error { return fn(tx, "") })
	case authctx.ActorShopAdmin:
		tenantID, err := shopAdminTenant(ctx)
		if err != nil {
			return err
		}
		return dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error { return fn(tx, tenantID) })
	case authctx.ActorVisitor, authctx.ActorCustomer, authctx.ActorService:
	}
	return ycerrors.Forbidden("Admins only").WithInternal("actor_type %s", a.ActorType)
}

// requireSystemAdmin refuses other callers.
func requireSystemAdmin(ctx context.Context) error {
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorSystemAdmin {
		return ycerrors.Forbidden("System admins only").WithInternal("actor_type %s", a.ActorType)
	}
	return nil
}

// shopAdminTenant returns the calling shop admin's shop.
func shopAdminTenant(ctx context.Context) (string, error) {
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorShopAdmin {
		return "", ycerrors.Forbidden("Shop admins only").WithInternal("actor_type %s", a.ActorType)
	}
	if a.TenantID == "" || a.TenantID == eventbus.PlatformTenantID {
		return "", ycerrors.Forbidden("Shop admins only").WithInternal("tenant %q", a.TenantID)
	}
	return a.TenantID, nil
}
