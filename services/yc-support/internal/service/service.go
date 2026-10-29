// Package service holds yc-support's use cases. Every mutation writes its
// tables and publishes its events in one transaction; notifications go out
// after it commits.
package service

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	"github.com/slaghuis/yc-platform/services/yc-support/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-support/internal/integration"
	"github.com/slaghuis/yc-platform/services/yc-support/internal/repository"
)

// ServiceName is the actor ID of events yc-support publishes on its own
// behalf.
const ServiceName = "yc-support"

// Publisher publishes through the outbox.
type Publisher interface {
	Publish(ctx context.Context, tx pgx.Tx, evt eventbus.Event) error
}

// Messages is yc-message's send API.
type Messages interface {
	Send(ctx context.Context, s integration.Send) error
}

// Tenants is yc-tenant's internal API.
type Tenants interface {
	ContactEmail(ctx context.Context, tenantID string) (string, error)
}

// Config holds the timings and links.
type Config struct {
	// IdleTTL is how long a request may go without a message before the
	// sweeper closes it.
	IdleTTL time.Duration
	// BatchSize bounds each sweep.
	BatchSize int
	// StorefrontBase prefixes the customer's thread link. Empty gives a
	// relative path, /account/support/{id}.
	StorefrontBase string
}

// DefaultIdleTTL is the idle window (docs/features/Customer support
// contact threads.md: 72 hours from last_activity_at).
const DefaultIdleTTL = 72 * time.Hour

// maxReferenceAttempts bounds retries when a random reference collides.
const maxReferenceAttempts = 8

// Service implements the support use cases.
type Service struct {
	db       *pgxpool.Pool
	bus      Publisher
	outbound Messages
	tenants  Tenants
	cfg      Config
	requests repository.Requests
	messages repository.Messages
	now      func() time.Time
}

// New returns a Service.
func New(db *pgxpool.Pool, bus Publisher, outbound Messages, tenants Tenants, cfg Config) *Service {
	if cfg.IdleTTL <= 0 {
		cfg.IdleTTL = DefaultIdleTTL
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 50
	}
	return &Service{db: db, bus: bus, outbound: outbound, tenants: tenants, cfg: cfg, now: time.Now}
}

// Page is one page of a list; Next is set when there is more.
type Page[T any] struct {
	Items []T
	Next  *repository.Cursor
}

// asCustomer runs fn for the calling customer in their shop.
func (s *Service) asCustomer(ctx context.Context, fn func(tx pgx.Tx, tenantID, userID string) error) error {
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorCustomer || a.ActorID == "" {
		return ycerrors.Forbidden("Sign in to see your support requests").WithInternal("actor_type %s", a.ActorType)
	}
	return s.inTenant(ctx, a.TenantID, func(tx pgx.Tx, tenantID string) error { return fn(tx, tenantID, a.ActorID) })
}

// asShopAdmin runs fn for the calling shop admin in their shop.
func (s *Service) asShopAdmin(ctx context.Context, fn func(tx pgx.Tx, tenantID, adminID string) error) error {
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorShopAdmin || a.ActorID == "" {
		return ycerrors.Forbidden("Shop admins only").WithInternal("actor_type %s", a.ActorType)
	}
	return s.inTenant(ctx, a.TenantID, func(tx pgx.Tx, tenantID string) error { return fn(tx, tenantID, a.ActorID) })
}

func (s *Service) inTenant(ctx context.Context, tenantID string, fn func(tx pgx.Tx, tenantID string) error) error {
	if err := validateShopTenant(tenantID); err != nil {
		return err
	}
	return dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error { return fn(tx, tenantID) })
}

// serviceContext acts as yc-support in tenantID, for background jobs and
// event handlers.
func serviceContext(ctx context.Context, tenantID string) context.Context {
	a, _ := authctx.FromContext(ctx)
	return authctx.WithAuth(ctx, authctx.AuthContext{
		TenantID: tenantID, ActorType: authctx.ActorService, ActorID: ServiceName, CorrelationID: a.CorrelationID,
	})
}

// publish attributes the event to the caller, or to yc-support itself when
// there is none (the sweeper, event handlers).
func (s *Service) publish(ctx context.Context, tx pgx.Tx, tenantID, eventType string, payload any) error {
	actor := eventbus.Actor{Type: string(authctx.ActorService), ID: ServiceName}
	if a, ok := authctx.FromContext(ctx); ok && a.ActorType != "" && a.ActorID != "" {
		actor = eventbus.Actor{Type: string(a.ActorType), ID: a.ActorID, AccessTokenJTI: a.AccessTokenJTI}
	}
	return s.bus.Publish(ctx, tx, eventbus.Event{EventType: eventType, TenantID: tenantID, Actor: actor, Payload: payload})
}

// requestID checks id looks like a request ID; anything else is no request.
func requestID(id string) (string, error) {
	u, err := uuid.Parse(id)
	if err != nil {
		return "", ycerrors.NotFound("Support request", id)
	}
	return u.String(), nil
}

func errNotFound(id string) error { return ycerrors.NotFound("Support request", id) }

func validateShopTenant(tenantID string) error {
	switch tenantID {
	case "":
		return ycerrors.Validation().Field("tenant_id", "required", "Tenant is required").Build()
	case eventbus.PlatformTenantID:
		return ycerrors.Validation().Field("tenant_id", "invalid", "Support requests belong to a shop, not "+tenantID).Build()
	}
	if err := eventbus.ValidateTenantID(tenantID); err != nil {
		return ycerrors.Validation().Field("tenant_id", "invalid", "Tenant ID is not valid").Wrap(err)
	}
	return nil
}

// page trims rows (fetched with limit+1) to a page and sets its cursor.
func page(rows []domain.Request, limit int) Page[domain.Request] {
	p := Page[domain.Request]{Items: rows}
	if len(rows) > limit {
		p.Items = rows[:limit]
		last := p.Items[limit-1]
		p.Next = &repository.Cursor{At: last.LastActivityAt, ID: last.ID}
	}
	return p
}
