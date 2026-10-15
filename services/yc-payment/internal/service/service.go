// Package service holds yc-payment's use cases. Gateway calls happen
// outside database transactions: a transaction row is written first, the
// gateway is called, and its answer is recorded with the matching event.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"

	"github.com/slaghuis/yc-platform/services/yc-payment/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-payment/internal/gateway"
	"github.com/slaghuis/yc-platform/services/yc-payment/internal/repository"
	"github.com/slaghuis/yc-platform/services/yc-payment/internal/secrets"
)

// ServiceName is the actor ID of events yc-payment publishes on its own
// behalf (webhooks).
const ServiceName = "yc-payment"

// Publisher publishes through the outbox.
type Publisher interface {
	Publish(ctx context.Context, tx pgx.Tx, evt eventbus.Event) error
}

// Service implements the payment use cases.
type Service struct {
	db      *pgxpool.Pool
	bus     Publisher
	repo    repository.Payments
	cipher  *secrets.Cipher
	drivers map[string]gateway.Driver
	now     func() time.Time
}

// New returns a Service for the given gateway drivers.
func New(db *pgxpool.Pool, bus Publisher, cipher *secrets.Cipher, drivers ...gateway.Driver) *Service {
	s := &Service{db: db, bus: bus, cipher: cipher, drivers: map[string]gateway.Driver{}, now: time.Now}
	for _, d := range drivers {
		s.drivers[d.Name()] = d
	}
	return s
}

func (s *Service) driver(name string) (gateway.Driver, error) {
	d, ok := s.drivers[name]
	if !ok {
		return nil, ycerrors.NotFound("gateway", name)
	}
	return d, nil
}

// GatewayConfigs returns the calling shop's gateway settings, without
// secrets.
func (s *Service) GatewayConfigs(ctx context.Context) ([]domain.GatewayConfig, error) {
	var out []domain.GatewayConfig
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		stored, err := s.repo.GatewayConfigs(ctx, tx, tenantID)
		out = make([]domain.GatewayConfig, 0, len(stored))
		for _, c := range stored {
			out = append(out, c.GatewayConfig)
		}
		return err
	})
	return out, err
}

// PutGatewayConfig is a shop's new settings for a gateway. An empty APIKey
// keeps the stored one; a nil WebhookSecret, Priority or Currencies keeps
// the stored one (or the default for a new gateway).
type PutGatewayConfig struct {
	Enabled       bool
	APIKey        string
	WebhookSecret *string
	PublicKey     string
	Priority      *int
	Currencies    *[]string
}

// PutGatewayConfig stores the calling shop's settings for gw, sealing the
// secrets under the cipher key.
func (s *Service) PutGatewayConfig(ctx context.Context, gw string, in PutGatewayConfig) (domain.GatewayConfig, error) {
	d, err := s.driver(gw)
	if err != nil {
		return domain.GatewayConfig{}, err
	}
	var out domain.GatewayConfig
	err = s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		creds := gateway.Credentials{APIKey: in.APIKey}
		existing, ok, err := s.repo.GatewayConfig(ctx, tx, tenantID, gw)
		if err != nil {
			return err
		}
		var old gateway.Credentials
		if ok {
			if old, err = s.open(tenantID, existing); err != nil {
				return err
			}
		}
		if creds.APIKey == "" {
			creds.APIKey = old.APIKey
		}
		creds.WebhookSecret = old.WebhookSecret
		if in.WebhookSecret != nil {
			creds.WebhookSecret = *in.WebhookSecret
		}
		priority, currencies := domain.DefaultPriority, []string{}
		if ok {
			priority, currencies = existing.Priority, existing.Currencies
		}
		if in.Priority != nil {
			priority = *in.Priority
		}
		if in.Currencies != nil {
			currencies = *in.Currencies
		}
		if err := errors.Join(domain.ValidateCredentials(creds, in.PublicKey),
			domain.ValidateRouting(priority, currencies, d.SupportedCurrencies())); err != nil {
			return mergeValidation(err)
		}
		plain, err := json.Marshal(creds) //nolint:gosec // sealed before it is stored
		if err != nil {
			return fmt.Errorf("encode credentials: %w", err)
		}
		sealed, err := s.cipher.Seal(plain, aad(tenantID, gw))
		if err != nil {
			return err
		}
		now := s.now().UTC()
		c := repository.StoredGatewayConfig{
			GatewayConfig: domain.GatewayConfig{
				Gateway: gw, Enabled: in.Enabled, APIKeyHint: domain.KeyHint(creds.APIKey),
				WebhookSecretSet: creds.WebhookSecret != "", PublicKey: in.PublicKey,
				Priority: priority, Currencies: currencies, CreatedAt: now, UpdatedAt: now,
			},
			CredentialsEnc: sealed,
		}
		if ok {
			c.CreatedAt = existing.CreatedAt
		}
		if err := s.repo.PutGatewayConfig(ctx, tx, tenantID, c); err != nil {
			return err
		}
		out = c.GatewayConfig
		return nil
	})
	return out, err
}

// TestGateway checks the calling shop's stored credentials against gw.
func (s *Service) TestGateway(ctx context.Context, gw string) error {
	d, err := s.driver(gw)
	if err != nil {
		return err
	}
	var creds gateway.Credentials
	err = s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		creds, err = s.credentials(ctx, tx, tenantID, gw, false)
		return err
	})
	if err != nil {
		return err
	}
	return d.Ping(ctx, creds)
}

// Capabilities describes a gateway for yc-checkout.
type Capabilities struct {
	Gateway                     string
	SupportsSeparateAuthCapture bool
	HostedPayment               bool
	// SupportedCurrencies is nil when the gateway takes any currency.
	SupportedCurrencies []string
	Configured          bool
	PublicKey           string
}

func capabilities(d gateway.Driver) Capabilities {
	return Capabilities{
		Gateway: d.Name(), SupportsSeparateAuthCapture: d.SupportsSeparateAuthCapture(), HostedPayment: d.HostedPayment(),
		SupportedCurrencies: d.SupportedCurrencies(),
	}
}

// Capabilities reports what gw can do and whether the calling shop has
// it set up.
func (s *Service) Capabilities(ctx context.Context, gw string) (Capabilities, error) {
	d, err := s.driver(gw)
	if err != nil {
		return Capabilities{}, err
	}
	out := capabilities(d)
	err = s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		c, ok, err := s.repo.GatewayConfig(ctx, tx, tenantID, gw)
		out.Configured = ok && c.Enabled
		out.PublicKey = c.PublicKey
		return err
	})
	return out, err
}

// Route picks the calling shop's gateway for a payment in currency
// (ADR-013): among enabled gateways that take the currency, the lowest
// priority. A named gateway must qualify itself. No such gateway is
// precondition_failed: the shop cannot take the payment.
func (s *Service) Route(ctx context.Context, currency, gw string) (Capabilities, error) {
	if !currencyRE.MatchString(currency) {
		return Capabilities{}, ycerrors.Validation().Field("currency", "invalid", "Currency must be an ISO 4217 code").Build()
	}
	if gw != "" {
		if _, err := s.driver(gw); err != nil {
			return Capabilities{}, err
		}
	}
	var out Capabilities
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		stored, err := s.repo.GatewayConfigs(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		configs := make([]domain.GatewayConfig, 0, len(stored))
		for _, c := range stored {
			if gw == "" || c.Gateway == gw {
				configs = append(configs, c.GatewayConfig)
			}
		}
		c, ok := domain.Route(configs, currency, func(name string) ([]string, bool) {
			d, ok := s.drivers[name]
			if !ok {
				return nil, false
			}
			return d.SupportedCurrencies(), true
		})
		if !ok {
			msg := "The shop takes no payments in " + currency
			if gw != "" {
				msg = "The shop does not take " + currency + " payments with " + gw
			}
			return ycerrors.PreconditionFailed(msg).With("currency", currency)
		}
		out = capabilities(s.drivers[c.Gateway])
		out.Configured, out.PublicKey = true, c.PublicKey
		return nil
	})
	return out, err
}

// mergeValidation folds several validation errors into one problem.
func mergeValidation(err error) error {
	var all []*ycerrors.Error
	for _, e := range unjoin(err) {
		var ye *ycerrors.Error
		if !errors.As(e, &ye) || ye.Code != ycerrors.CodeValidationFailed {
			return err
		}
		all = append(all, ye)
	}
	if len(all) == 1 {
		return all[0]
	}
	v := ycerrors.Validation()
	for _, e := range all {
		for _, f := range e.Fields {
			v.Field(f.Field, f.Code, f.Message)
		}
	}
	return v.Build()
}

func unjoin(err error) []error {
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		return j.Unwrap()
	}
	return []error{err}
}

// credentials opens tenantID's credentials for gw. A shop without them,
// or (with requireEnabled) with the gateway switched off, cannot take
// payments.
func (s *Service) credentials(ctx context.Context, tx pgx.Tx, tenantID, gw string, requireEnabled bool) (gateway.Credentials, error) {
	c, ok, err := s.repo.GatewayConfig(ctx, tx, tenantID, gw)
	if err != nil {
		return gateway.Credentials{}, err
	}
	if !ok || (requireEnabled && !c.Enabled) {
		return gateway.Credentials{}, ycerrors.PreconditionFailed("The shop has not set up " + gw + " payments")
	}
	return s.open(tenantID, c)
}

func (s *Service) open(tenantID string, c repository.StoredGatewayConfig) (gateway.Credentials, error) {
	plain, err := s.cipher.Open(c.CredentialsEnc, aad(tenantID, c.Gateway))
	if err != nil {
		return gateway.Credentials{}, ycerrors.Internal("gateway credentials do not decrypt; was the cipher key changed?").Wrap(err)
	}
	var creds gateway.Credentials
	if err := json.Unmarshal(plain, &creds); err != nil {
		return gateway.Credentials{}, ycerrors.Internal("decode gateway credentials").Wrap(err)
	}
	return creds, nil
}

func aad(tenantID, gw string) []byte { return []byte(tenantID + "|" + gw) }

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
	if err := validateShopTenant(a.TenantID); err != nil {
		return err
	}
	return dbkit.WithTenantTx(ctx, s.db, a.TenantID, func(tx pgx.Tx) error { return fn(tx, a.TenantID) })
}

// publish attributes the event to the caller, or to yc-payment itself
// when there is none (webhooks).
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
		return ycerrors.Validation().Field("tenant_id", "invalid", "Payments belong to a shop, not "+tenantID).Build()
	}
	if err := eventbus.ValidateTenantID(tenantID); err != nil {
		return ycerrors.Validation().Field("tenant_id", "invalid", "Tenant ID is not valid").Wrap(err)
	}
	return nil
}
