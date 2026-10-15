// Package service holds yc-cart's use cases. Every mutation writes its
// tables and publishes its events in one transaction; prices are looked up
// before the transaction starts.
package service

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	money "github.com/slaghuis/yc-platform/libs/yc-money"
	"github.com/slaghuis/yc-platform/services/yc-cart/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-cart/internal/repository"
)

// ServiceName is the actor ID of events yc-cart publishes on its own
// behalf.
const ServiceName = "yc-cart"

// Publisher publishes through the outbox.
type Publisher interface {
	Publish(ctx context.Context, tx pgx.Tx, evt eventbus.Event) error
}

// Prices looks up current prices (yc-pricing).
type Prices interface {
	UnitPrices(ctx context.Context, skuIDs []string, displayCurrency string) (domain.Prices, error)
}

// Service implements the cart use cases.
type Service struct {
	db     *pgxpool.Pool
	bus    Publisher
	prices Prices
	repo   repository.Carts
	types  domain.Types
	now    func() time.Time
}

// New returns a Service for the configured cart types.
func New(db *pgxpool.Pool, bus Publisher, prices Prices, types domain.Types) *Service {
	return &Service{db: db, bus: bus, prices: prices, types: types, now: time.Now}
}

// caller is the cart owner behind a request.
type caller struct {
	tenantID, userID string
	actorType        authctx.ActorType
}

// Summary describes one of the caller's carts without pricing it.
type Summary struct {
	Type     domain.CartType
	CartID   string
	Lines    int
	Quantity int
}

// List summarises the caller's carts, one per configured type.
func (s *Service) List(ctx context.Context) ([]Summary, error) {
	c, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	var carts []domain.Cart
	err = dbkit.WithTenantTx(ctx, s.db, c.tenantID, func(tx pgx.Tx) error {
		carts, err = s.repo.List(ctx, tx, c.tenantID, c.userID)
		return err
	})
	if err != nil {
		return nil, err
	}
	byType := make(map[string]domain.Cart, len(carts))
	for _, cart := range carts {
		byType[cart.Type] = cart
	}
	out := make([]Summary, 0, len(s.types))
	for _, name := range s.types.Names() {
		cart := byType[name]
		out = append(out, Summary{Type: s.types[name], CartID: cart.ID, Lines: len(cart.Items), Quantity: cart.Quantity()})
	}
	return out, nil
}

// Get returns the caller's cart of cartType priced now. displayCurrency
// overrides the customer's preferred one.
func (s *Service) Get(ctx context.Context, cartType, displayCurrency string) (domain.View, error) {
	c, err := s.caller(ctx)
	if err != nil {
		return domain.View{}, err
	}
	t, err := s.types.Get(cartType)
	if err != nil {
		return domain.View{}, err
	}
	var cart domain.Cart
	err = dbkit.WithTenantTx(ctx, s.db, c.tenantID, func(tx pgx.Tx) error {
		cart, _, err = s.repo.Get(ctx, tx, c.tenantID, c.userID, cartType, false)
		if err != nil || displayCurrency != "" || c.actorType != authctx.ActorCustomer {
			return err
		}
		displayCurrency, err = s.repo.DisplayCurrency(ctx, tx, c.tenantID, c.userID)
		return err
	})
	if err != nil {
		return domain.View{}, err
	}
	cart.Type = cartType
	return s.price(ctx, cart, t, displayCurrency)
}

func (s *Service) price(ctx context.Context, cart domain.Cart, t domain.CartType, displayCurrency string) (domain.View, error) {
	if len(cart.Items) == 0 {
		return domain.View{Cart: cart, Type: t}, nil
	}
	ids := make([]string, len(cart.Items))
	for i, it := range cart.Items {
		ids[i] = it.SKUID
	}
	p, err := s.prices.UnitPrices(ctx, ids, displayCurrency)
	if err != nil {
		return domain.View{}, err
	}
	return domain.Price(cart, t, p)
}

// AddItem adds quantity of skuID to the caller's cart of cartType,
// snapshotting its current price on a new line, and returns the cart.
func (s *Service) AddItem(ctx context.Context, cartType, skuID string, quantity int, displayCurrency string) (domain.View, error) {
	c, err := s.caller(ctx)
	if err != nil {
		return domain.View{}, err
	}
	t, err := s.types.Get(cartType)
	if err != nil {
		return domain.View{}, err
	}
	if err := t.CheckQuantity(quantity); err != nil {
		return domain.View{}, err
	}
	unit, err := s.unitPrice(ctx, skuID)
	if err != nil {
		return domain.View{}, err
	}
	err = dbkit.WithTenantTx(ctx, s.db, c.tenantID, func(tx pgx.Tx) error {
		cart, err := s.ensureCart(ctx, tx, c, cartType)
		if err != nil {
			return err
		}
		now := s.now()
		it, exists := cart.Item(skuID)
		if !exists {
			if len(cart.Items) >= t.MaxItems {
				return ycerrors.Conflict(fmt.Sprintf("The %s cart holds at most %d items", cartType, t.MaxItems))
			}
			it = domain.Item{SKUID: skuID, UnitPrice: unit, AddedAt: now}
		}
		if it.Quantity+quantity > t.MaxQuantity {
			return ycerrors.Conflict(fmt.Sprintf("At most %d of an item fit in the %s cart", t.MaxQuantity, cartType))
		}
		it.Quantity += quantity
		if err := s.writeLine(ctx, tx, c.tenantID, cart.ID, it, now); err != nil {
			return err
		}
		return s.publish(ctx, tx, c.tenantID, events.TypeCartItemAddedV1, events.CartItemAddedV1{
			CartID: cart.ID, UserID: c.userID, CartType: cartType, SKUID: skuID,
			QuantityAdded: int64(quantity), Quantity: int64(it.Quantity), UnitPrice: eventMoney(it.UnitPrice),
			AddedAt: now.UTC(),
		})
	})
	if err != nil {
		return domain.View{}, err
	}
	return s.Get(ctx, cartType, displayCurrency)
}

// unitPrice is skuID's current gross unit price; SKUs without one cannot
// be added.
func (s *Service) unitPrice(ctx context.Context, skuID string) (money.Money, error) {
	p, err := s.prices.UnitPrices(ctx, []string{skuID}, "")
	if err != nil {
		return money.Money{}, err
	}
	if unit, ok := p.Units[skuID]; ok {
		return unit, nil
	}
	if p.Unpriced[skuID] == "unknown_sku" {
		return money.Money{}, ycerrors.NotFound("SKU", skuID)
	}
	return money.Money{}, ycerrors.Conflict("This item is not for sale").WithInternal("unpriced: %s", p.Unpriced[skuID])
}

// SetQuantity sets a line's quantity and returns the cart.
func (s *Service) SetQuantity(ctx context.Context, cartType, skuID string, quantity int, displayCurrency string) (domain.View, error) {
	c, err := s.caller(ctx)
	if err != nil {
		return domain.View{}, err
	}
	t, err := s.types.Get(cartType)
	if err != nil {
		return domain.View{}, err
	}
	if err := t.CheckQuantity(quantity); err != nil {
		return domain.View{}, err
	}
	err = s.withLine(ctx, c, cartType, skuID, func(tx pgx.Tx, cart domain.Cart, it domain.Item) error {
		if it.Quantity == quantity {
			return nil
		}
		now := s.now()
		prev := it.Quantity
		it.Quantity = quantity
		if err := s.writeLine(ctx, tx, c.tenantID, cart.ID, it, now); err != nil {
			return err
		}
		return s.publish(ctx, tx, c.tenantID, events.TypeCartItemQuantityChangedV1, events.CartItemQuantityChangedV1{
			CartID: cart.ID, UserID: c.userID, CartType: cartType, SKUID: skuID,
			PreviousQuantity: int64(prev), Quantity: int64(quantity), ChangedAt: now.UTC(),
		})
	})
	if err != nil {
		return domain.View{}, err
	}
	return s.Get(ctx, cartType, displayCurrency)
}

// RemoveItem removes a line.
func (s *Service) RemoveItem(ctx context.Context, cartType, skuID string) error {
	c, err := s.caller(ctx)
	if err != nil {
		return err
	}
	if _, err := s.types.Get(cartType); err != nil {
		return err
	}
	return s.withLine(ctx, c, cartType, skuID, func(tx pgx.Tx, cart domain.Cart, it domain.Item) error {
		return s.removeLine(ctx, tx, c.tenantID, cart, it, events.CartItemRemovedV1ReasonCustomer)
	})
}

func (s *Service) removeLine(ctx context.Context, tx pgx.Tx, tenantID string, cart domain.Cart, it domain.Item, reason string) error {
	now := s.now()
	if err := s.repo.DeleteItem(ctx, tx, cart.ID, it.SKUID); err != nil {
		return err
	}
	if err := s.repo.Touch(ctx, tx, cart.ID, now); err != nil {
		return err
	}
	return s.publish(ctx, tx, tenantID, events.TypeCartItemRemovedV1, events.CartItemRemovedV1{
		CartID: cart.ID, UserID: cart.UserID, CartType: cart.Type, SKUID: it.SKUID,
		Quantity: int64(it.Quantity), Reason: reason, RemovedAt: now.UTC(),
	})
}

// MoveItem moves quantity of a line (all of it when 0) to the caller's
// cart of type to, and returns that cart.
func (s *Service) MoveItem(ctx context.Context, from, skuID, to string, quantity int, displayCurrency string) (domain.View, error) {
	c, err := s.caller(ctx)
	if err != nil {
		return domain.View{}, err
	}
	if _, err := s.types.Get(from); err != nil {
		return domain.View{}, err
	}
	dest, err := s.types.Get(to)
	if err != nil {
		return domain.View{}, err
	}
	if from == to {
		return domain.View{}, ycerrors.Validation().Field("to_cart_type", "invalid", "Must differ from the source cart").Build()
	}
	if quantity < 0 {
		return domain.View{}, ycerrors.Validation().Field("quantity", "out_of_range", "Must be positive").Build()
	}
	err = s.withLine(ctx, c, from, skuID, func(tx pgx.Tx, src domain.Cart, it domain.Item) error {
		if quantity == 0 {
			quantity = it.Quantity
		}
		if quantity > it.Quantity {
			return ycerrors.Validation().Field("quantity", "out_of_range",
				fmt.Sprintf("The line holds %d", it.Quantity)).Build()
		}
		dst, err := s.ensureCart(ctx, tx, c, to)
		if err != nil {
			return err
		}
		now := s.now()
		moved, exists := dst.Item(skuID)
		if !exists {
			if len(dst.Items) >= dest.MaxItems {
				return ycerrors.Conflict(fmt.Sprintf("The %s cart holds at most %d items", to, dest.MaxItems))
			}
			moved = domain.Item{SKUID: skuID, UnitPrice: it.UnitPrice, AddedAt: now}
		}
		if moved.Quantity+quantity > dest.MaxQuantity {
			return ycerrors.Conflict(fmt.Sprintf("At most %d of an item fit in the %s cart", dest.MaxQuantity, to))
		}
		moved.Quantity += quantity
		if err := s.writeLine(ctx, tx, c.tenantID, dst.ID, moved, now); err != nil {
			return err
		}
		if it.Quantity -= quantity; it.Quantity == 0 {
			err = s.repo.DeleteItem(ctx, tx, src.ID, skuID)
		} else {
			err = s.repo.PutItem(ctx, tx, c.tenantID, src.ID, it, now)
		}
		if err != nil {
			return err
		}
		if err := s.repo.Touch(ctx, tx, src.ID, now); err != nil {
			return err
		}
		return s.publish(ctx, tx, c.tenantID, events.TypeCartItemMovedV1, events.CartItemMovedV1{
			UserID: c.userID, SKUID: skuID, FromCartID: src.ID, FromCartType: from, ToCartID: dst.ID, ToCartType: to,
			Quantity: int64(quantity), MovedAt: now.UTC(),
		})
	})
	if err != nil {
		return domain.View{}, err
	}
	return s.Get(ctx, to, displayCurrency)
}

// Clear empties the caller's cart of cartType.
func (s *Service) Clear(ctx context.Context, cartType string) error {
	c, err := s.caller(ctx)
	if err != nil {
		return err
	}
	if _, err := s.types.Get(cartType); err != nil {
		return err
	}
	return dbkit.WithTenantTx(ctx, s.db, c.tenantID, func(tx pgx.Tx) error {
		cart, ok, err := s.repo.Get(ctx, tx, c.tenantID, c.userID, cartType, true)
		if err != nil || !ok || len(cart.Items) == 0 {
			return err
		}
		now := s.now()
		if err := s.repo.Clear(ctx, tx, cart.ID); err != nil {
			return err
		}
		if err := s.repo.Touch(ctx, tx, cart.ID, now); err != nil {
			return err
		}
		return s.publish(ctx, tx, c.tenantID, events.TypeCartClearedV1, events.CartClearedV1{
			CartID: cart.ID, UserID: c.userID, CartType: cartType, ItemCount: int64(len(cart.Items)),
			Reason: events.CartClearedV1ReasonCustomer, ClearedAt: now.UTC(),
		})
	})
}

// MergeVisitor merges visitorID's carts into userID's after a login
// (identity.user.logged_in.v1), then deletes them. Merged visitor carts are
// gone, so redelivery finds nothing to do.
func (s *Service) MergeVisitor(ctx context.Context, tenantID, userID, visitorID string) error {
	if visitorID == "" || visitorID == userID {
		return nil
	}
	if err := validateShopTenant(tenantID); err != nil {
		return err
	}
	c := caller{tenantID: tenantID, userID: userID, actorType: authctx.ActorCustomer}
	return dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		for _, name := range s.types.Names() {
			t := s.types[name]
			if !t.MergeOnLogin {
				continue
			}
			if err := s.mergeCart(ctx, tx, c, visitorID, t); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Service) mergeCart(ctx context.Context, tx pgx.Tx, c caller, visitorID string, t domain.CartType) error {
	from, ok, err := s.repo.Get(ctx, tx, c.tenantID, visitorID, t.Name, true)
	if err != nil || !ok {
		return err
	}
	if len(from.Items) == 0 {
		return s.repo.Delete(ctx, tx, from.ID)
	}
	into, err := s.ensureCart(ctx, tx, c, t.Name)
	if err != nil {
		return err
	}
	writes, merged := domain.MergeItems(into.Items, from.Items, t)
	now := s.now()
	for _, it := range writes {
		if err := s.repo.PutItem(ctx, tx, c.tenantID, into.ID, it, now); err != nil {
			return err
		}
	}
	if err := s.repo.Touch(ctx, tx, into.ID, now); err != nil {
		return err
	}
	if err := s.repo.Delete(ctx, tx, from.ID); err != nil {
		return err
	}
	return s.publish(ctx, tx, c.tenantID, events.TypeCartMergedV1, events.CartMergedV1{
		CartID: into.ID, UserID: c.userID, VisitorID: visitorID, CartType: t.Name,
		MergedItemCount: int64(merged), MergedAt: now.UTC(),
	})
}

// SetDisplayCurrency records a customer's display currency
// (preference.currency.changed.v1).
func (s *Service) SetDisplayCurrency(ctx context.Context, tenantID, userID, currency string, at time.Time) error {
	if err := validateShopTenant(tenantID); err != nil {
		return err
	}
	if !money.ValidCurrency(currency) {
		return ycerrors.Validation().Field("display_currency", "invalid", "Not a currency code").Build()
	}
	return dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		return s.repo.SetDisplayCurrency(ctx, tx, tenantID, userID, currency, at)
	})
}

// InternalCart returns userID's cart of cartType for a service caller
// (yc-checkout). A cart not created yet has no ID and no lines.
func (s *Service) InternalCart(ctx context.Context, userID, cartType string) (domain.Cart, domain.CartType, error) {
	a, _ := authctx.FromContext(ctx)
	if err := validateShopTenant(a.TenantID); err != nil {
		return domain.Cart{}, domain.CartType{}, err
	}
	t, err := s.types.Get(cartType)
	if err != nil {
		return domain.Cart{}, domain.CartType{}, err
	}
	cart := domain.Cart{UserID: userID, Type: cartType}
	err = dbkit.WithTenantTx(ctx, s.db, a.TenantID, func(tx pgx.Tx) error {
		c, ok, err := s.repo.Get(ctx, tx, a.TenantID, userID, cartType, false)
		if ok {
			cart = c
		}
		return err
	})
	return cart, t, err
}

// ConvertCart removes the quantities of a completed checkout
// (checkout.completed.v1) from userID's cart of cartType.
func (s *Service) ConvertCart(ctx context.Context, tenantID, userID, cartType, sessionID, orderID string, checkedOut map[string]int) error {
	if err := validateShopTenant(tenantID); err != nil {
		return err
	}
	return dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		now := s.now()
		first, err := s.repo.RecordConversion(ctx, tx, tenantID, sessionID, orderID, now)
		if err != nil || !first {
			return err
		}
		cart, ok, err := s.repo.Get(ctx, tx, tenantID, userID, cartType, true)
		if err != nil || !ok {
			return err
		}
		writes, deletes, removed := domain.ConvertItems(cart.Items, checkedOut)
		if len(removed) == 0 {
			return nil
		}
		for _, it := range writes {
			if err := s.repo.PutItem(ctx, tx, tenantID, cart.ID, it, now); err != nil {
				return err
			}
		}
		for _, sku := range deletes {
			if err := s.repo.DeleteItem(ctx, tx, cart.ID, sku); err != nil {
				return err
			}
		}
		if err := s.repo.Touch(ctx, tx, cart.ID, now); err != nil {
			return err
		}
		items := make([]events.CartConvertedV1ItemsItem, 0, len(removed))
		for _, it := range cart.Items {
			if q, ok := removed[it.SKUID]; ok {
				items = append(items, events.CartConvertedV1ItemsItem{SKUID: it.SKUID, Quantity: int64(q)})
			}
		}
		return s.publish(ctx, tx, tenantID, events.TypeCartConvertedV1, events.CartConvertedV1{
			CartID: cart.ID, UserID: userID, CartType: cartType, SessionID: sessionID, OrderID: orderID,
			Items: items, ConvertedAt: now.UTC(),
		})
	})
}

// RemoveSKUs removes deleted SKUs from every cart in the shop.
func (s *Service) RemoveSKUs(ctx context.Context, tenantID string, skuIDs []string) error {
	if err := validateShopTenant(tenantID); err != nil {
		return err
	}
	return dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		lines, err := s.repo.LinesWithSKUs(ctx, tx, tenantID, skuIDs)
		if err != nil {
			return err
		}
		for _, l := range lines {
			if err := s.removeLine(ctx, tx, tenantID, l.Cart, l.Item, events.CartItemRemovedV1ReasonSKUDeleted); err != nil {
				return err
			}
		}
		return nil
	})
}

// withLine runs fn with the caller's locked cart of cartType and its line
// for skuID, which must exist.
func (s *Service) withLine(ctx context.Context, c caller, cartType, skuID string, fn func(tx pgx.Tx, cart domain.Cart, it domain.Item) error) error {
	return dbkit.WithTenantTx(ctx, s.db, c.tenantID, func(tx pgx.Tx) error {
		cart, ok, err := s.repo.Get(ctx, tx, c.tenantID, c.userID, cartType, true)
		if err != nil {
			return err
		}
		it, has := cart.Item(skuID)
		if !ok || !has {
			return ycerrors.NotFound("Cart item", skuID)
		}
		return fn(tx, cart, it)
	})
}

// ensureCart returns the caller's locked cart of cartType, creating it
// (and announcing it) if needed.
func (s *Service) ensureCart(ctx context.Context, tx pgx.Tx, c caller, cartType string) (domain.Cart, error) {
	now := s.now()
	created, err := s.repo.Create(ctx, tx, c.tenantID, c.userID, cartType, now)
	if err != nil {
		return domain.Cart{}, err
	}
	cart, _, err := s.repo.Get(ctx, tx, c.tenantID, c.userID, cartType, true)
	if err != nil || !created {
		return cart, err
	}
	userType := events.CartCreatedV1UserTypeCustomer
	if c.actorType == authctx.ActorVisitor {
		userType = events.CartCreatedV1UserTypeVisitor
	}
	return cart, s.publish(ctx, tx, c.tenantID, events.TypeCartCreatedV1, events.CartCreatedV1{
		CartID: cart.ID, UserID: c.userID, UserType: userType, CartType: cartType, CreatedAt: now.UTC(),
	})
}

func (s *Service) writeLine(ctx context.Context, tx pgx.Tx, tenantID, cartID string, it domain.Item, now time.Time) error {
	if err := s.repo.PutItem(ctx, tx, tenantID, cartID, it, now); err != nil {
		return err
	}
	return s.repo.Touch(ctx, tx, cartID, now)
}

// caller returns the cart owner: a visitor or customer in a shop.
func (s *Service) caller(ctx context.Context) (caller, error) {
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorVisitor && a.ActorType != authctx.ActorCustomer {
		return caller{}, ycerrors.Forbidden("Carts are for shoppers").WithInternal("actor_type %s", a.ActorType)
	}
	if err := validateShopTenant(a.TenantID); err != nil {
		return caller{}, err
	}
	return caller{tenantID: a.TenantID, userID: a.ActorID, actorType: a.ActorType}, nil
}

// publish attributes the event to the caller, or to yc-cart itself when
// there is none (event handlers).
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
		return ycerrors.Validation().Field("tenant_id", "invalid", "Carts belong to a shop, not "+tenantID).Build()
	}
	if err := eventbus.ValidateTenantID(tenantID); err != nil {
		return ycerrors.Validation().Field("tenant_id", "invalid", "Tenant ID is not valid").Wrap(err)
	}
	return nil
}

func eventMoney(m money.Money) events.Money {
	return events.Money{AmountMinor: m.AmountMinor, Currency: m.Currency}
}
