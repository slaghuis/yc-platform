// Package service holds yc-pricing's use cases. Every mutation writes its
// tables and publishes its event in one transaction.
package service

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	money "github.com/slaghuis/yc-platform/libs/yc-money"
	"github.com/slaghuis/yc-platform/services/yc-pricing/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-pricing/internal/repository"
)

// ServiceName is the actor ID of events yc-pricing publishes on its own
// behalf.
const ServiceName = "yc-pricing"

// maxHistory caps the price history returned for a SKU.
const maxHistory = 50

// Publisher publishes through the outbox.
type Publisher interface {
	Publish(ctx context.Context, tx pgx.Tx, evt eventbus.Event) error
}

// Service implements the pricing use cases.
type Service struct {
	db       *pgxpool.Pool
	bus      Publisher
	repo     repository.Pricing
	pipeline domain.Pipeline
	now      func() time.Time
}

// New returns a Service.
func New(db *pgxpool.Pool, bus Publisher) *Service {
	return &Service{db: db, bus: bus, pipeline: domain.DefaultPipeline(), now: time.Now}
}

// GetConfig returns the shop's configuration.
func (s *Service) GetConfig(ctx context.Context) (domain.Config, error) {
	var c domain.Config
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		c, err = s.repo.Config(ctx, tx, tenantID, false)
		return err
	})
	return c, err
}

// PutConfig replaces the shop's configuration. The store currency cannot
// change once prices exist (ADR-014).
func (s *Service) PutConfig(ctx context.Context, in domain.ConfigInput) (domain.Config, error) {
	if err := in.Validate(); err != nil {
		return domain.Config{}, err
	}
	var c domain.Config
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		old, err := s.repo.Config(ctx, tx, tenantID, true)
		switch {
		case ycerrors.HasCode(err, ycerrors.CodeNotFound):
		case err != nil:
			return err
		case old.StoreCurrency != in.StoreCurrency:
			n, err := s.repo.CountPrices(ctx, tx)
			if err != nil {
				return err
			}
			if n > 0 {
				return ycerrors.Conflict("The store currency cannot change while base prices exist")
			}
		case old.TaxInclusive == in.TaxInclusive && old.DefaultJurisdiction == in.DefaultJurisdiction:
			c = old
			return nil
		}
		if c, err = s.repo.PutConfig(ctx, tx, tenantID, in, s.now()); err != nil {
			return err
		}
		return s.publishConfig(ctx, tx, c)
	})
	return c, err
}

func (s *Service) publishConfig(ctx context.Context, tx pgx.Tx, c domain.Config) error {
	return s.publish(ctx, tx, c.TenantID, events.TypePricingConfigChangedV1, events.PricingConfigChangedV1{
		StoreCurrency: c.StoreCurrency, IsTaxInclusive: c.TaxInclusive,
		DefaultJurisdiction: c.DefaultJurisdiction, ChangedAt: c.UpdatedAt.UTC(),
	})
}

// SetPrice sets a SKU's base price in the store currency, interpreted with
// the shop's current tax-inclusive setting. An unchanged price is a no-op.
func (s *Service) SetPrice(ctx context.Context, skuID string, price money.Money) (domain.BasePrice, error) {
	var p domain.BasePrice
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		cfg, err := s.configured(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		if err := domain.ValidatePrice(price, cfg.StoreCurrency); err != nil {
			return err
		}
		prev, err := s.repo.CurrentPrice(ctx, tx, skuID)
		if err != nil {
			return err
		}
		if prev != nil && prev.Price == price && prev.TaxInclusive == cfg.TaxInclusive {
			p = *prev
			return nil
		}
		a, _ := authctx.FromContext(ctx)
		if p, err = s.repo.SetPrice(ctx, tx, tenantID, skuID, price, cfg.TaxInclusive, s.now(), a.ActorID); err != nil {
			return err
		}
		evt := events.PricingBasePriceSetV1{
			SKUID: skuID, Price: eventMoney(p.Price), IsTaxInclusive: p.TaxInclusive, ValidFrom: p.ValidFrom.UTC(),
		}
		if prev != nil {
			pm := eventMoney(prev.Price)
			evt.PreviousPrice = &pm
		}
		return s.publish(ctx, tx, tenantID, events.TypePricingBasePriceSetV1, evt)
	})
	return p, err
}

// RemovePrice removes a SKU's base price, leaving it unpriced.
func (s *Service) RemovePrice(ctx context.Context, skuID string) error {
	return s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		return s.removePrice(ctx, tx, tenantID, skuID, true)
	})
}

func (s *Service) removePrice(ctx context.Context, tx pgx.Tx, tenantID, skuID string, mustExist bool) error {
	now := s.now()
	removed, err := s.repo.RemovePrice(ctx, tx, skuID, now)
	if err != nil {
		return err
	}
	if !removed {
		if mustExist {
			return ycerrors.NotFound("Base price", skuID)
		}
		return nil
	}
	return s.publish(ctx, tx, tenantID, events.TypePricingBasePriceRemovedV1, events.PricingBasePriceRemovedV1{
		SKUID: skuID, RemovedAt: now.UTC(),
	})
}

// PriceHistory returns a SKU's prices, newest first.
func (s *Service) PriceHistory(ctx context.Context, skuID string) ([]domain.PricePeriod, error) {
	var out []domain.PricePeriod
	err := s.inShop(ctx, func(tx pgx.Tx, _ string) error {
		var err error
		out, err = s.repo.PriceHistory(ctx, tx, skuID, maxHistory)
		return err
	})
	if err == nil && len(out) == 0 {
		err = ycerrors.NotFound("Base price", skuID)
	}
	return out, err
}

// ListPrices lists the shop's current prices by SKU ID.
func (s *Service) ListPrices(ctx context.Context, afterSKU string, limit int) ([]domain.BasePrice, string, error) {
	var out []domain.BasePrice
	err := s.inShop(ctx, func(tx pgx.Tx, _ string) error {
		var err error
		out, err = s.repo.ListPrices(ctx, tx, afterSKU, limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	var next string
	if len(out) > limit {
		out = out[:limit]
		next = out[limit-1].SKUID
	}
	return out, next, nil
}

// ListTaxClasses lists the shop's tax classes by ID.
func (s *Service) ListTaxClasses(ctx context.Context) ([]domain.TaxClass, error) {
	var classes map[string]domain.TaxClass
	err := s.inShop(ctx, func(tx pgx.Tx, _ string) error {
		var err error
		classes, err = s.repo.TaxClasses(ctx, tx, nil)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := make([]domain.TaxClass, 0, len(classes))
	for _, c := range classes {
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b domain.TaxClass) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// GetTaxClass returns a tax class with its rules.
func (s *Service) GetTaxClass(ctx context.Context, id string) (domain.TaxClass, error) {
	var c domain.TaxClass
	err := s.inShop(ctx, func(tx pgx.Tx, _ string) error {
		var err error
		c, err = s.repo.TaxClass(ctx, tx, id)
		return err
	})
	return c, err
}

// PutTaxClass creates or replaces a tax class, reporting whether it was
// created.
func (s *Service) PutTaxClass(ctx context.Context, id string, in domain.TaxClassInput) (domain.TaxClass, bool, error) {
	if err := domain.ValidateClassID(id); err != nil {
		return domain.TaxClass{}, false, err
	}
	rules, err := in.Validate()
	if err != nil {
		return domain.TaxClass{}, false, err
	}
	var (
		c       domain.TaxClass
		created bool
	)
	err = s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		if c, created, err = s.repo.PutTaxClass(ctx, tx, tenantID, id, in.DisplayName, rules, s.now()); err != nil {
			return err
		}
		if created {
			return s.publish(ctx, tx, tenantID, events.TypePricingTaxClassCreatedV1, events.PricingTaxClassCreatedV1{
				ClassID: c.ID, DisplayName: c.DisplayName, Rules: eventRules[events.PricingTaxClassCreatedV1RulesItem](c.Rules),
				CreatedAt: c.CreatedAt.UTC(),
			})
		}
		return s.publish(ctx, tx, tenantID, events.TypePricingTaxClassUpdatedV1, events.PricingTaxClassUpdatedV1{
			ClassID: c.ID, DisplayName: c.DisplayName, Rules: eventRules[events.PricingTaxClassUpdatedV1RulesItem](c.Rules),
			UpdatedAt: c.UpdatedAt.UTC(),
		})
	})
	return c, created, err
}

// DeleteTaxClass deletes a tax class no live SKU uses.
func (s *Service) DeleteTaxClass(ctx context.Context, id string) error {
	return s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		n, err := s.repo.CountSKUsInClass(ctx, tx, id)
		if err != nil {
			return err
		}
		if n > 0 {
			return ycerrors.Conflict("The tax class is used by SKUs")
		}
		if err := s.repo.DeleteTaxClass(ctx, tx, id); err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypePricingTaxClassDeletedV1, events.PricingTaxClassDeletedV1{
			ClassID: id, DeletedAt: s.now().UTC(),
		})
	})
}

// Evaluate prices req for the caller's shop. It reads only, so any actor
// may call it.
func (s *Service) Evaluate(ctx context.Context, req domain.Request) (domain.Result, error) {
	if err := req.Validate(); err != nil {
		return domain.Result{}, err
	}
	a, _ := authctx.FromContext(ctx)
	if err := validateShopTenant(a.TenantID); err != nil {
		return domain.Result{}, err
	}
	in := &domain.Inputs{}
	err := dbkit.WithTenantTx(ctx, s.db, a.TenantID, func(tx pgx.Tx) error {
		var err error
		if in.Config, err = s.configured(ctx, tx, a.TenantID); err != nil {
			return err
		}
		ids := req.SKUIDs()
		if in.Prices, err = s.repo.Prices(ctx, tx, ids); err != nil {
			return err
		}
		if in.TaxClasses, err = s.repo.SKUTaxClasses(ctx, tx, ids); err != nil {
			return err
		}
		classIDs := make([]string, 0, len(in.TaxClasses))
		for _, c := range in.TaxClasses {
			if !slices.Contains(classIDs, c) {
				classIDs = append(classIDs, c)
			}
		}
		if in.Classes, err = s.repo.TaxClasses(ctx, tx, classIDs); err != nil {
			return err
		}
		if req.DisplayCurrency != "" && req.DisplayCurrency != in.Config.StoreCurrency {
			in.Rate, err = s.repo.Rate(ctx, tx, in.Config.StoreCurrency, req.DisplayCurrency)
		}
		return err
	})
	if err != nil {
		return domain.Result{}, err
	}
	return s.pipeline.Run(req, in, s.now())
}

// configured returns the shop's configuration, which must exist.
func (s *Service) configured(ctx context.Context, tx pgx.Tx, tenantID string) (domain.Config, error) {
	cfg, err := s.repo.Config(ctx, tx, tenantID, false)
	if ycerrors.HasCode(err, ycerrors.CodeNotFound) {
		return domain.Config{}, ycerrors.Conflict("Pricing is not configured for this shop").Wrap(err)
	}
	return cfg, err
}

// inShop runs fn in the caller's shop. Pricing administration is for shop
// admins only.
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

// publish attributes the event to the caller, or to yc-pricing itself when
// there is none (event handlers, the rate refresher).
func (s *Service) publish(ctx context.Context, tx pgx.Tx, tenantID, eventType string, payload any) error {
	return publish(ctx, s.bus, tx, tenantID, eventType, payload)
}

func publish(ctx context.Context, bus Publisher, tx pgx.Tx, tenantID, eventType string, payload any) error {
	actor := eventbus.Actor{Type: string(authctx.ActorService), ID: ServiceName}
	if a, ok := authctx.FromContext(ctx); ok && a.ActorType != "" {
		actor = eventbus.Actor{Type: string(a.ActorType), ID: a.ActorID, AccessTokenJTI: a.AccessTokenJTI}
	}
	return bus.Publish(ctx, tx, eventbus.Event{EventType: eventType, TenantID: tenantID, Actor: actor, Payload: payload})
}

func validateShopTenant(tenantID string) error {
	switch tenantID {
	case "":
		return ycerrors.Validation().Field("tenant_id", "required", "Tenant is required").Build()
	case eventbus.PlatformTenantID:
		return ycerrors.Validation().Field("tenant_id", "invalid", "Prices belong to a shop, not "+tenantID).Build()
	}
	if err := eventbus.ValidateTenantID(tenantID); err != nil {
		return ycerrors.Validation().Field("tenant_id", "invalid", "Tenant ID is not valid").Wrap(err)
	}
	return nil
}

func eventMoney(m money.Money) events.Money {
	return events.Money{AmountMinor: m.AmountMinor, Currency: m.Currency}
}

type ruleItem interface {
	events.PricingTaxClassCreatedV1RulesItem | events.PricingTaxClassUpdatedV1RulesItem
}

func eventRules[T ruleItem](rules []domain.TaxRule) []T {
	out := make([]T, len(rules))
	for i, r := range rules {
		out[i] = T{Jurisdiction: r.Jurisdiction, RuleName: r.Name, Rate: r.Rate.String(), Base: r.Base, SortOrder: int64(r.SortOrder)}
	}
	return out
}
