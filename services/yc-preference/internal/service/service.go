// Package service holds yc-preference's use cases. Every mutation writes
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
	"github.com/slaghuis/yc-platform/services/yc-preference/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-preference/internal/repository"
)

// ServiceName is the actor ID of events yc-preference publishes on its own
// behalf.
const ServiceName = "yc-preference"

// Publisher publishes through the outbox.
type Publisher interface {
	Publish(ctx context.Context, tx pgx.Tx, evt eventbus.Event) error
}

// Service implements the preference use cases.
type Service struct {
	db       *pgxpool.Pool
	bus      Publisher
	repo     repository.Preferences
	messages Messages
	fallback domain.Defaults
	now      func() time.Time
}

// New returns a Service. fallback applies to shops with no defaults of
// their own.
func New(db *pgxpool.Pool, bus Publisher, messages Messages, fallback domain.Defaults) *Service {
	return &Service{db: db, bus: bus, messages: messages, fallback: fallback, now: time.Now}
}

// Get returns the caller's preferences, or the shop's defaults when they
// have none stored.
func (s *Service) Get(ctx context.Context) (domain.Preferences, error) {
	var p domain.Preferences
	err := s.asUser(ctx, func(tx pgx.Tx, tenantID, userID string) error {
		stored, ok, err := s.repo.Get(ctx, tx, tenantID, userID, false)
		if err != nil || ok {
			p = stored
			return err
		}
		d, err := s.defaults(ctx, tx, tenantID)
		p = prefsFromDefaults(userID, d)
		return err
	})
	return p, err
}

// Update applies patch to the caller's preferences, publishing an event
// per changed setting.
func (s *Service) Update(ctx context.Context, patch domain.Patch) (domain.Preferences, error) {
	if err := patch.Validate(); err != nil {
		return domain.Preferences{}, err
	}
	var p domain.Preferences
	err := s.asUser(ctx, func(tx pgx.Tx, tenantID, userID string) error {
		old, err := s.ensure(ctx, tx, tenantID, userID)
		if err != nil {
			return err
		}
		p = patch.Apply(old)
		if p.PrimaryChannel != old.PrimaryChannel {
			if _, ok := p.Channel(p.PrimaryChannel); !ok {
				return ycerrors.Validation().Field("primary_channel", "unknown", "Add the channel before making it primary").Build()
			}
		}
		unchanged := p.DisplayCurrency == old.DisplayCurrency && p.Locale == old.Locale &&
			p.PrimaryChannel == old.PrimaryChannel && domain.MarketingEqual(p, old)
		if unchanged {
			return nil
		}
		now := s.now()
		if err := s.repo.Update(ctx, tx, tenantID, p, now); err != nil {
			return err
		}
		p.UpdatedAt = now
		return s.publishChanges(ctx, tx, tenantID, old, p, now)
	})
	return p, err
}

func (s *Service) publishChanges(ctx context.Context, tx pgx.Tx, tenantID string, old, p domain.Preferences, now time.Time) error {
	at := now.UTC()
	if p.DisplayCurrency != old.DisplayCurrency {
		err := s.publish(ctx, tx, tenantID, events.TypePreferenceCurrencyChangedV1, events.PreferenceCurrencyChangedV1{
			UserID: p.UserID, DisplayCurrency: p.DisplayCurrency, PreviousDisplayCurrency: &old.DisplayCurrency, ChangedAt: at,
		})
		if err != nil {
			return err
		}
	}
	if p.Locale != old.Locale {
		err := s.publish(ctx, tx, tenantID, events.TypePreferenceLocaleChangedV1, events.PreferenceLocaleChangedV1{
			UserID: p.UserID, Locale: p.Locale, PreviousLocale: &old.Locale, ChangedAt: at,
		})
		if err != nil {
			return err
		}
	}
	if !domain.MarketingEqual(p, old) {
		err := s.publish(ctx, tx, tenantID, events.TypePreferenceMarketingChangedV1, events.PreferenceMarketingChangedV1{
			UserID: p.UserID, Newsletter: p.Newsletter, Marketing: p.Marketing, NewProducts: p.NewProducts,
			PreviousNewsletter: &old.Newsletter, PreviousMarketing: &old.Marketing, PreviousNewProducts: &old.NewProducts,
			ChangedAt: at,
		})
		if err != nil {
			return err
		}
	}
	if p.PrimaryChannel != old.PrimaryChannel {
		return s.publishPrimary(ctx, tx, tenantID, p.UserID, p.PrimaryChannel, old.PrimaryChannel, now)
	}
	return nil
}

func (s *Service) publishPrimary(ctx context.Context, tx pgx.Tx, tenantID, userID string, c, prev domain.Channel, now time.Time) error {
	evt := events.PreferenceChannelChangedV1{UserID: userID, PrimaryChannel: string(c), ChangedAt: now.UTC()}
	if prev != "" {
		p := string(prev)
		evt.PreviousPrimaryChannel = &p
	}
	return s.publish(ctx, tx, tenantID, events.TypePreferenceChannelChangedV1, evt)
}

// PutChannel adds a messaging channel, or replaces its address, reporting
// whether it was added. A new address must be verified again.
func (s *Service) PutChannel(ctx context.Context, c domain.Channel, address string) (domain.MessagingChannel, bool, error) {
	address, err := domain.NormaliseAddress(c, address)
	if err != nil {
		return domain.MessagingChannel{}, false, err
	}
	var (
		mc      domain.MessagingChannel
		created bool
	)
	err = s.asUser(ctx, func(tx pgx.Tx, tenantID, userID string) error {
		p, err := s.ensure(ctx, tx, tenantID, userID)
		if err != nil {
			return err
		}
		old, exists := p.Channel(c)
		if exists && old.Address == address {
			mc = old
			return nil
		}
		created = !exists
		mc, err = s.putChannel(ctx, tx, tenantID, userID, c, address)
		return err
	})
	return mc, created, err
}

func (s *Service) putChannel(ctx context.Context, tx pgx.Tx, tenantID, userID string, c domain.Channel, address string) (domain.MessagingChannel, error) {
	now := s.now()
	if err := s.repo.PutChannel(ctx, tx, tenantID, userID, c, address, now); err != nil {
		return domain.MessagingChannel{}, err
	}
	err := s.publish(ctx, tx, tenantID, events.TypePreferenceChannelAddedV1, events.PreferenceChannelAddedV1{
		UserID: userID, Channel: string(c), Address: address, AddedAt: now.UTC(),
	})
	if err != nil {
		return domain.MessagingChannel{}, err
	}
	p, _, err := s.repo.Get(ctx, tx, tenantID, userID, false)
	if err != nil {
		return domain.MessagingChannel{}, err
	}
	mc, _ := p.Channel(c)
	return mc, nil
}

// DeleteChannel removes a messaging channel. The primary channel cannot be
// removed until another is chosen.
func (s *Service) DeleteChannel(ctx context.Context, c domain.Channel) error {
	if !c.Valid() {
		return ycerrors.NotFound("Channel", string(c))
	}
	return s.asUser(ctx, func(tx pgx.Tx, tenantID, userID string) error {
		p, ok, err := s.repo.Get(ctx, tx, tenantID, userID, true)
		if err != nil {
			return err
		}
		if _, has := p.Channel(c); !ok || !has {
			return ycerrors.NotFound("Channel", string(c))
		}
		if p.PrimaryChannel == c {
			return ycerrors.Conflict("Choose another primary channel before removing this one")
		}
		if err := s.repo.DeleteChannel(ctx, tx, tenantID, userID, c); err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypePreferenceChannelRemovedV1, events.PreferenceChannelRemovedV1{
			UserID: userID, Channel: string(c), RemovedAt: s.now().UTC(),
		})
	})
}

// GetDefaults returns the caller's shop defaults.
func (s *Service) GetDefaults(ctx context.Context) (domain.Defaults, error) {
	var d domain.Defaults
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		d, err = s.defaults(ctx, tx, tenantID)
		return err
	})
	return d, err
}

// PutDefaults replaces the caller's shop defaults. They apply to users
// without stored preferences; stored preferences keep their values.
func (s *Service) PutDefaults(ctx context.Context, d domain.Defaults) (domain.Defaults, error) {
	if err := d.Validate(); err != nil {
		return domain.Defaults{}, err
	}
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		d.UpdatedAt = s.now()
		return s.repo.PutDefaults(ctx, tx, tenantID, d, d.UpdatedAt)
	})
	return d, err
}

// InitShop gives a newly provisioned shop defaults: its store currency and
// the platform locale. Existing defaults are kept.
func (s *Service) InitShop(ctx context.Context, tenantID, storeCurrency string) error {
	if err := validateShopTenant(tenantID); err != nil {
		return err
	}
	d := domain.Defaults{DisplayCurrency: storeCurrency, Locale: s.fallback.Locale}
	if err := d.Validate(); err != nil {
		return err
	}
	return dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		return s.repo.InitDefaults(ctx, tx, tenantID, d, s.now())
	})
}

// SeedUser stores a newly registered user's preferences from the shop
// defaults, with their login email as the primary channel. Each step is
// skipped if already done, so redelivery is harmless.
func (s *Service) SeedUser(ctx context.Context, tenantID, userID, email string) error {
	if err := validateShopTenant(tenantID); err != nil {
		return err
	}
	var address string
	if email != "" {
		var err error
		address, err = domain.NormaliseAddress(domain.ChannelEmail, email)
		if err != nil {
			return err
		}
	}
	return dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		p, err := s.ensure(ctx, tx, tenantID, userID)
		if err != nil {
			return err
		}
		// invitation_accepted.v1 carries no email (ADR-030); seed shop defaults
		// without a channel. Registration always passes the login email.
		if address == "" {
			return nil
		}
		if _, ok := p.Channel(domain.ChannelEmail); !ok {
			if _, err := s.putChannel(ctx, tx, tenantID, userID, domain.ChannelEmail, address); err != nil {
				return err
			}
		}
		if p.PrimaryChannel != "" {
			return nil
		}
		now := s.now()
		p.PrimaryChannel = domain.ChannelEmail
		if err := s.repo.Update(ctx, tx, tenantID, p, now); err != nil {
			return err
		}
		return s.publishPrimary(ctx, tx, tenantID, userID, domain.ChannelEmail, "", now)
	})
}

// ensure returns userID's stored preferences, locked, first storing the
// shop defaults (and announcing the display currency) if there are none.
func (s *Service) ensure(ctx context.Context, tx pgx.Tx, tenantID, userID string) (domain.Preferences, error) {
	p, ok, err := s.repo.Get(ctx, tx, tenantID, userID, true)
	if err != nil || ok {
		return p, err
	}
	d, err := s.defaults(ctx, tx, tenantID)
	if err != nil {
		return domain.Preferences{}, err
	}
	now := s.now()
	p = prefsFromDefaults(userID, d)
	inserted, err := s.repo.Insert(ctx, tx, tenantID, p, now)
	if err != nil {
		return domain.Preferences{}, err
	}
	if inserted {
		err := s.publish(ctx, tx, tenantID, events.TypePreferenceCurrencyChangedV1, events.PreferenceCurrencyChangedV1{
			UserID: userID, DisplayCurrency: p.DisplayCurrency, ChangedAt: now.UTC(),
		})
		if err != nil {
			return domain.Preferences{}, err
		}
	}
	p, _, err = s.repo.Get(ctx, tx, tenantID, userID, true)
	return p, err
}

func (s *Service) defaults(ctx context.Context, tx pgx.Tx, tenantID string) (domain.Defaults, error) {
	d, ok, err := s.repo.Defaults(ctx, tx, tenantID)
	if err != nil || ok {
		return d, err
	}
	return s.fallback, nil
}

func prefsFromDefaults(userID string, d domain.Defaults) domain.Preferences {
	return domain.Preferences{
		UserID: userID, DisplayCurrency: d.DisplayCurrency, Locale: d.Locale,
		Newsletter: d.Newsletter, Marketing: d.Marketing, NewProducts: d.NewProducts,
	}
}

// asUser runs fn for the calling customer or shop admin in their shop.
func (s *Service) asUser(ctx context.Context, fn func(tx pgx.Tx, tenantID, userID string) error) error {
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorCustomer && a.ActorType != authctx.ActorShopAdmin {
		return ycerrors.Forbidden("Sign in to a shop to keep preferences").WithInternal("actor_type %s", a.ActorType)
	}
	if err := validateShopTenant(a.TenantID); err != nil {
		return err
	}
	return dbkit.WithTenantTx(ctx, s.db, a.TenantID, func(tx pgx.Tx) error { return fn(tx, a.TenantID, a.ActorID) })
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

// publish attributes the event to the caller, or to yc-preference itself
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
		return ycerrors.Validation().Field("tenant_id", "invalid", "Preferences belong to a shop, not "+tenantID).Build()
	}
	if err := eventbus.ValidateTenantID(tenantID); err != nil {
		return ycerrors.Validation().Field("tenant_id", "invalid", "Tenant ID is not valid").Wrap(err)
	}
	return nil
}
