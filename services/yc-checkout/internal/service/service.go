// Package service holds yc-checkout's use cases and the saga that drives
// a confirmed session through its participants (ADR-019, Variant A).
// Every state change writes the session, its history and its events in
// one transaction.
package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"

	"github.com/slaghuis/yc-platform/services/yc-checkout/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-checkout/internal/integration"
	"github.com/slaghuis/yc-platform/services/yc-checkout/internal/repository"
)

// ServiceName is the actor ID of work yc-checkout does on its own behalf.
const ServiceName = "yc-checkout"

// Publisher publishes through the outbox.
type Publisher interface {
	Publish(ctx context.Context, tx pgx.Tx, evt eventbus.Event) error
}

// Participants are the services the saga calls (integration.Clients).
type Participants interface {
	Cart(ctx context.Context, userID, cartType string) (integration.Cart, error)
	SKUs(ctx context.Context, ids []string) (integration.SKUBatch, error)
	Evaluate(ctx context.Context, lines []integration.CartLine) (integration.Pricing, error)
	Reserve(ctx context.Context, key, sessionID string, lines []integration.CartLine, ttlMinutes int) (integration.Reservation, error)
	Commit(ctx context.Context, key, reservationID, reference string) error
	Release(ctx context.Context, key, reservationID string) error
	Authorize(ctx context.Context, key string, req integration.AuthorizeRequest) (integration.Transaction, error)
	Capture(ctx context.Context, key, transactionID, orderID string) (integration.Transaction, error)
	Void(ctx context.Context, key, transactionID, authorizationKey string) (string, error)
	Transaction(ctx context.Context, id string) (integration.Transaction, error)
	CreateOrder(ctx context.Context, key string, in integration.CreateOrder) (integration.Order, error)
	ConfirmOrder(ctx context.Context, key, orderID, transactionID string) (integration.Order, error)
	AbandonOrder(ctx context.Context, key, orderID, reason string) error
}

// Settings tune sessions and the saga.
type Settings struct {
	// SessionTTL is how long the quote is honoured (ADR-011).
	SessionTTL time.Duration
	// ReservationTTL is asked of yc-inventory (ADR-012).
	ReservationTTL time.Duration
	// Shipping is the one flat-fee option offered for physical items. It
	// is untaxed.
	Shipping domain.ShippingOption
	// Gateways are the payment gateways a session may use; the first is
	// the default.
	Gateways []string
	// ConfirmDeadline bounds one run of the saga; confirm then answers 202
	// and the sweeper carries on.
	ConfirmDeadline time.Duration
	// LeaseTTL is how long a driver holds a session without saving it.
	LeaseTTL time.Duration
	// RetryBase and RetryMax bound the backoff between background
	// attempts.
	RetryBase time.Duration
	RetryMax  time.Duration
	// PendingPoll is how often a payment awaiting the customer is
	// re-checked when no webhook event arrives.
	PendingPoll time.Duration
	// AlertAfter is the number of failed background attempts after which
	// a stuck session is logged as critical.
	AlertAfter int
	// PauseBefore and PauseFor stall the saga before a step (development
	// only), e.g. to kill the service mid-saga.
	PauseBefore domain.State
	PauseFor    time.Duration
	// Host identifies this replica across restarts.
	Host string
}

// Service implements the checkout use cases.
type Service struct {
	db    *pgxpool.Pool
	bus   Publisher
	p     Participants
	cfg   Settings
	repo  repository.Sessions
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
	// run identifies this process; lease tokens start with it.
	runID string
}

// New returns a Service.
func New(db *pgxpool.Pool, bus Publisher, p Participants, cfg Settings) *Service {
	if len(cfg.Gateways) == 0 {
		cfg.Gateways = []string{"stripe"}
	}
	if cfg.Host == "" {
		cfg.Host = ServiceName
	}
	return &Service{
		db: db, bus: bus, p: p, cfg: cfg, now: time.Now, sleep: sleepCtx,
		runID: cfg.Host + "/" + uuid.NewString()[:8] + "/",
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// errLeaseLost: another driver took the session over.
var errLeaseLost = errors.New("checkout: session lease lost")

func requireActor(ctx context.Context, types ...authctx.ActorType) (authctx.AuthContext, error) {
	a, _ := authctx.FromContext(ctx)
	for _, t := range types {
		if a.ActorType == t {
			return a, nil
		}
	}
	return a, ycerrors.Forbidden("Not permitted").WithInternal("actor_type %s", a.ActorType)
}

// inShop runs fn in the caller's shop. Routes decide who may call.
func (s *Service) inShop(ctx context.Context, fn func(tx pgx.Tx, tenantID string) error) error {
	a, _ := authctx.FromContext(ctx)
	if err := validateShopTenant(a.TenantID); err != nil {
		return err
	}
	return dbkit.WithTenantTx(ctx, s.db, a.TenantID, func(tx pgx.Tx) error { return fn(tx, a.TenantID) })
}

// load reads a session outside any lease.
func (s *Service) load(ctx context.Context, tenantID, id string) (domain.Session, error) {
	var sess domain.Session
	err := dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		var ok bool
		var err error
		sess, ok, err = s.repo.Session(ctx, tx, tenantID, id, false)
		if err == nil && !ok {
			err = ycerrors.NotFound("checkout session", id)
		}
		return err
	})
	return sess, err
}

// publish attributes the event to the caller, or to yc-checkout itself.
func (s *Service) publish(ctx context.Context, tx pgx.Tx, tenantID, eventType string, payload any) error {
	actor := eventbus.Actor{Type: string(authctx.ActorService), ID: ServiceName}
	if a, ok := authctx.FromContext(ctx); ok && a.ActorType != "" {
		actor = eventbus.Actor{Type: string(a.ActorType), ID: a.ActorID, AccessTokenJTI: a.AccessTokenJTI}
	}
	return s.bus.Publish(ctx, tx, eventbus.Event{EventType: eventType, TenantID: tenantID, Actor: actor, Payload: payload})
}

// history appends a session event.
func (s *Service) history(ctx context.Context, tx pgx.Tx, tenantID, sessionID, eventType string, from, to domain.State, payload map[string]any) error {
	return s.repo.AddEvent(ctx, tx, tenantID, sessionID, repository.Event{
		EventType: eventType, From: from, To: to, Payload: payload, CreatedAt: s.now().UTC(),
	})
}

// serviceContext is the context background work calls participants with:
// the session's tenant, acting as yc-checkout.
func serviceContext(ctx context.Context, tenantID string) context.Context {
	a, _ := authctx.FromContext(ctx)
	return authctx.WithAuth(ctx, authctx.AuthContext{
		TenantID: tenantID, ActorType: authctx.ActorService, ActorID: ServiceName, CorrelationID: a.CorrelationID,
	})
}

func validateShopTenant(tenantID string) error {
	switch tenantID {
	case "":
		return ycerrors.Validation().Field("tenant_id", "required", "Tenant is required").Build()
	case eventbus.PlatformTenantID:
		return ycerrors.Validation().Field("tenant_id", "invalid", "Checkout belongs to a shop, not "+tenantID).Build()
	}
	if err := eventbus.ValidateTenantID(tenantID); err != nil {
		return ycerrors.Validation().Field("tenant_id", "invalid", "Tenant ID is not valid").Wrap(err)
	}
	return nil
}

func isNotFound(err error) bool { return ycerrors.HasCode(err, ycerrors.CodeNotFound) }

func minutes(d time.Duration) int {
	return max(1, int(d/time.Minute))
}

func describe(err error) map[string]any {
	if r, ok := integration.Rejected(err); ok {
		out := map[string]any{"status": r.Status, "code": string(r.Problem.Code)}
		if r.Problem.Detail != "" {
			out["detail"] = r.Problem.Detail
		}
		return out
	}
	return map[string]any{"error": fmt.Sprint(err)}
}
