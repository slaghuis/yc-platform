// Package service holds yc-message's use cases: turning events and send
// requests into messages, delivering them, verifying channels and managing
// templates (ADR-023).
package service

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	"github.com/slaghuis/yc-platform/services/yc-message/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-message/internal/driver"
	"github.com/slaghuis/yc-platform/services/yc-message/internal/integration"
	"github.com/slaghuis/yc-platform/services/yc-message/internal/repository"
)

// ServiceName is the actor ID of events yc-message publishes on its own
// behalf.
const ServiceName = "yc-message"

// Publisher publishes through the outbox.
type Publisher interface {
	Publish(ctx context.Context, tx pgx.Tx, evt eventbus.Event) error
}

// Upstream is what yc-message reads from other services.
type Upstream interface {
	Preferences(ctx context.Context, userID string) (integration.Preferences, error)
	Order(ctx context.Context, orderID string) (integration.Order, error)
}

// Config tunes the service.
type Config struct {
	// Locale is used when the recipient's is unknown.
	Locale string
	// ShopURL is a shop's storefront; "{tenant}" is replaced by the shop's
	// tenant ID. Links in messages are built on it.
	ShopURL         string
	MaxAttempts     int
	RetryBackoff    time.Duration
	BatchSize       int
	VerificationTTL time.Duration
	// FakeWebhook accepts delivery reports on POST /webhooks/fake
	// (development only).
	FakeWebhook bool
	// SystemAdminEmails receive platform alerts.
	SystemAdminEmails []string
}

// Service implements the messaging use cases.
type Service struct {
	db       *pgxpool.Pool
	bus      Publisher
	upstream Upstream
	drivers  map[domain.Channel]driver.Driver
	repo     repository.Messages
	cfg      Config
	now      func() time.Time
	wake     chan struct{}
}

// New returns a Service with the given channel drivers.
func New(db *pgxpool.Pool, bus Publisher, upstream Upstream, cfg Config, drivers ...driver.Driver) *Service {
	s := &Service{
		db: db, bus: bus, upstream: upstream, cfg: cfg, now: time.Now,
		drivers: map[domain.Channel]driver.Driver{}, wake: make(chan struct{}, 1),
	}
	for _, d := range drivers {
		s.drivers[d.Channel()] = d
	}
	return s
}

func (s *Service) hasDriver(ch domain.Channel) bool {
	_, ok := s.drivers[ch]
	return ok
}

// request is a message to queue.
type request struct {
	Type      domain.Type
	Reference string
	To        domain.Recipient
	Context   map[string]any
}

// queue renders a message and stores it for the dispatcher. A reference
// the shop has already used returns that message, unchanged.
func (s *Service) queue(ctx context.Context, tx pgx.Tx, tenantID string, r request) (domain.Message, error) {
	if m, ok, err := s.repo.MessageByReference(ctx, tx, tenantID, r.Reference); err != nil || ok {
		return m, err
	}
	if !s.hasDriver(r.To.Channel) {
		return domain.Message{}, ycerrors.Validation().
			Field("channel", "unsupported", "Messages cannot be sent by "+string(r.To.Channel)+" yet").Build()
	}
	locale := r.To.Locale
	if locale == "" {
		locale = s.cfg.Locale
	}
	tpl, err := s.template(ctx, tx, tenantID, r.Type, r.To.Channel, locale)
	if err != nil {
		return domain.Message{}, err
	}
	msgCtx := map[string]any{"shop_url": s.links(ctx, tenantID).Home()}
	for k, v := range r.Context {
		msgCtx[k] = v
	}
	rendered, err := tpl.Render(msgCtx)
	if err != nil {
		return domain.Message{}, ycerrors.Validation().
			Field("context", "invalid", "The "+string(r.Type)+" template does not render: "+err.Error()).Build()
	}
	now := s.now().UTC()
	m := domain.Message{
		ID: uuid.NewString(), TenantID: tenantID, Type: r.Type, UserID: r.To.UserID, Address: r.To.Address,
		Channel: r.To.Channel, Locale: tpl.Locale, Rendered: rendered, Status: domain.StatusQueued,
		Driver: s.drivers[r.To.Channel].Name(), NextAttemptAt: &now, Context: msgCtx, Reference: r.Reference,
		CreatedAt: now, UpdatedAt: now,
	}
	if _, err := s.repo.InsertMessage(ctx, tx, m); err != nil {
		return domain.Message{}, err
	}
	s.kick()
	return m, nil
}

// template returns the shop's template for the locale, its language or
// the default locale, falling back to the platform's built-in one.
func (s *Service) template(ctx context.Context, tx pgx.Tx, tenantID string, t domain.Type, ch domain.Channel, locale string) (domain.Template, error) {
	for _, l := range domain.LocaleCandidates(locale, domain.DefaultLocale) {
		tpl, ok, err := s.repo.Template(ctx, tx, tenantID, t, ch, l)
		if err != nil || ok {
			return tpl, err
		}
		if l == domain.DefaultLocale {
			if tpl, ok := domain.Default(t, ch); ok {
				return tpl, nil
			}
		}
	}
	return domain.Template{}, ycerrors.NotFound("template", string(t)+"/"+string(ch))
}

// customer resolves a customer's channel from yc-preference. ok is false
// when they have no usable address.
func (s *Service) customer(ctx context.Context, userID string) (domain.Recipient, bool, error) {
	p, err := s.upstream.Preferences(ctx, userID)
	if httpkit.UpstreamStatus(err) == http.StatusNotFound {
		return domain.Recipient{}, false, nil
	}
	if err != nil {
		return domain.Recipient{}, false, err
	}
	c := domain.Contact{UserID: userID, Primary: domain.Channel(p.PrimaryChannel), Locale: p.Locale}
	for _, ch := range p.Channels {
		c.Channels = append(c.Channels, domain.ChannelAddress{
			Channel: domain.Channel(ch.Channel), Address: ch.Address, Verified: ch.Verified,
		})
	}
	r, ok := c.Resolve(s.hasDriver)
	return r, ok, nil
}

// kick wakes the dispatcher.
func (s *Service) kick() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// asShopAdmin runs fn in the calling shop admin's shop.
func (s *Service) asShopAdmin(ctx context.Context, fn func(tx pgx.Tx, tenantID string) error) error {
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorShopAdmin {
		return ycerrors.Forbidden("Shop admins only").WithInternal("actor_type %s", a.ActorType)
	}
	return s.inTenant(ctx, a.TenantID, fn)
}

// inShop runs fn in the caller's shop. Routes decide who may call.
func (s *Service) inShop(ctx context.Context, fn func(tx pgx.Tx, tenantID string) error) error {
	return s.inTenant(ctx, callerTenant(ctx), fn)
}

func callerTenant(ctx context.Context) string {
	a, _ := authctx.FromContext(ctx)
	return a.TenantID
}

func (s *Service) inTenant(ctx context.Context, tenantID string, fn func(tx pgx.Tx, tenantID string) error) error {
	if err := validateShopTenant(tenantID); err != nil {
		return err
	}
	return dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error { return fn(tx, tenantID) })
}

// serviceContext is the context event handlers and the dispatcher call
// other services with: the shop, acting as yc-message.
func serviceContext(ctx context.Context, tenantID string) context.Context {
	a, _ := authctx.FromContext(ctx)
	return authctx.WithAuth(ctx, authctx.AuthContext{
		TenantID: tenantID, ActorType: authctx.ActorService, ActorID: ServiceName, CorrelationID: a.CorrelationID,
	})
}

// publish attributes the event to yc-message.
func (s *Service) publish(ctx context.Context, tx pgx.Tx, tenantID, eventType string, payload any) error {
	return s.bus.Publish(ctx, tx, eventbus.Event{
		EventType: eventType, TenantID: tenantID,
		Actor:   eventbus.Actor{Type: string(authctx.ActorService), ID: ServiceName},
		Payload: payload,
	})
}

func validateShopTenant(tenantID string) error {
	switch tenantID {
	case "":
		return ycerrors.Validation().Field("tenant_id", "required", "Tenant is required").Build()
	case eventbus.PlatformTenantID:
		return ycerrors.Validation().Field("tenant_id", "invalid", "Messages belong to a shop, not "+tenantID).Build()
	}
	if err := eventbus.ValidateTenantID(tenantID); err != nil {
		return ycerrors.Validation().Field("tenant_id", "invalid", "Tenant ID is not valid").Wrap(err)
	}
	return nil
}

var errNoDriver = errors.New("no driver for the channel")
