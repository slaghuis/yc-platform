// Package service holds yc-review's use cases. Every mutation writes its
// tables and publishes its events in one transaction.
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
	"github.com/slaghuis/yc-platform/services/yc-review/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-review/internal/integration"
	"github.com/slaghuis/yc-platform/services/yc-review/internal/repository"
)

// ServiceName is the actor ID of events yc-review publishes on its own
// behalf.
const ServiceName = "yc-review"

// Publisher publishes through the outbox.
type Publisher interface {
	Publish(ctx context.Context, tx pgx.Tx, evt eventbus.Event) error
}

// Orders is yc-orders' internal API.
type Orders interface {
	LatestPurchase(ctx context.Context, userID, productID string) (integration.Purchase, error)
	Order(ctx context.Context, orderID string) (integration.Order, error)
}

// Config holds the defaults and timings.
type Config struct {
	// Defaults apply to shops without settings of their own.
	Defaults domain.Settings
	// CheckWindow is how long an unconfirmed purchase is retried before
	// the review is rejected; CheckRetry is the time between tries.
	CheckWindow time.Duration
	CheckRetry  time.Duration
	// PromptDelay is the time from delivery to the review prompt.
	PromptDelay time.Duration
	// BatchSize bounds each background sweep.
	BatchSize int
}

// Service implements the review use cases.
type Service struct {
	db     *pgxpool.Pool
	bus    Publisher
	orders Orders
	cfg    Config
	repo   repository.Reviews
	now    func() time.Time
}

// New returns a Service.
func New(db *pgxpool.Pool, bus Publisher, orders Orders, cfg Config) *Service {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 50
	}
	return &Service{db: db, bus: bus, orders: orders, cfg: cfg, now: time.Now}
}

// settings returns the shop's settings or the defaults.
func (s *Service) settings(ctx context.Context, tx pgx.Tx, tenantID string) (domain.Settings, error) {
	set, ok, err := s.repo.Settings(ctx, tx, tenantID)
	if err != nil || ok {
		return set, err
	}
	return s.cfg.Defaults, nil
}

// asCustomer runs fn in the calling customer's shop.
func (s *Service) asCustomer(ctx context.Context, fn func(tx pgx.Tx, tenantID, userID string) error) error {
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorCustomer || a.ActorID == "" {
		return ycerrors.Forbidden("Sign in to review and like products").WithInternal("actor_type %s", a.ActorType)
	}
	return s.inShop(ctx, func(tx pgx.Tx, tenantID string) error { return fn(tx, tenantID, a.ActorID) })
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

// serviceContext acts as yc-review in tenantID, for background jobs and
// event handlers.
func serviceContext(ctx context.Context, tenantID string) context.Context {
	a, _ := authctx.FromContext(ctx)
	return authctx.WithAuth(ctx, authctx.AuthContext{
		TenantID: tenantID, ActorType: authctx.ActorService, ActorID: ServiceName, CorrelationID: a.CorrelationID,
	})
}

// publish attributes the event to the caller, or to yc-review itself when
// there is none.
func (s *Service) publish(ctx context.Context, tx pgx.Tx, tenantID, eventType string, payload any) error {
	actor := eventbus.Actor{Type: string(authctx.ActorService), ID: ServiceName}
	if a, ok := authctx.FromContext(ctx); ok && a.ActorType != "" {
		actor = eventbus.Actor{Type: string(a.ActorType), ID: a.ActorID, AccessTokenJTI: a.AccessTokenJTI}
	}
	return s.bus.Publish(ctx, tx, eventbus.Event{EventType: eventType, TenantID: tenantID, Actor: actor, Payload: payload})
}

func productStats(st domain.Stats) events.ProductReviewStats {
	return events.ProductReviewStats{
		RatingCount: st.RatingCount, RatingSum: st.RatingSum, LikeCount: st.LikeCount, Version: st.Version,
	}
}

func validateShopTenant(tenantID string) error {
	switch tenantID {
	case "":
		return ycerrors.Validation().Field("tenant_id", "required", "Tenant is required").Build()
	case eventbus.PlatformTenantID:
		return ycerrors.Validation().Field("tenant_id", "invalid", "Reviews belong to a shop, not "+tenantID).Build()
	}
	if err := eventbus.ValidateTenantID(tenantID); err != nil {
		return ycerrors.Validation().Field("tenant_id", "invalid", "Tenant ID is not valid").Wrap(err)
	}
	return nil
}
