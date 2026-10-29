package service

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"

	"github.com/slaghuis/yc-platform/services/yc-checkout/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-checkout/internal/integration"
	"github.com/slaghuis/yc-platform/services/yc-checkout/internal/repository"
)

// DefaultCartType is checked out when the request names none.
const DefaultCartType = "shopping"

// Start snapshots the caller's cart, prices it with yc-pricing and opens
// a session in PRICING. The quote holds until the session expires
// (ADR-011).
func (s *Service) Start(ctx context.Context, cartType string) (domain.Session, error) {
	a, err := requireActor(ctx, authctx.ActorCustomer, authctx.ActorVisitor)
	if err != nil {
		return domain.Session{}, err
	}
	if err := validateShopTenant(a.TenantID); err != nil {
		return domain.Session{}, err
	}
	if cartType == "" {
		cartType = DefaultCartType
	}
	sess, err := s.quote(ctx, a, cartType)
	if err != nil {
		return sess, err
	}
	err = s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		if err := s.repo.InsertSession(ctx, tx, tenantID, sess); err != nil {
			return err
		}
		if err := s.history(ctx, tx, tenantID, sess.ID, "started", "", sess.State, map[string]any{
			"cart_type": sess.CartType, "items": len(sess.Items),
		}); err != nil {
			return err
		}
		if err := s.publish(ctx, tx, tenantID, events.TypeCheckoutStartedV1, events.CheckoutStartedV1{
			SessionID: sess.ID, UserID: sess.UserID, UserType: string(sess.UserType), CartType: sess.CartType,
			ItemCount: int64(len(sess.Items)), ExpiresAt: sess.ExpiresAt, StartedAt: sess.CreatedAt,
		}); err != nil {
			return err
		}
		m := moneyIn(sess.Currency)
		return s.publish(ctx, tx, tenantID, events.TypeCheckoutPricedV1, events.CheckoutPricedV1{
			SessionID: sess.ID, UserID: sess.UserID, ItemsTotal: m(sess.ItemsTotalMinor), TaxTotal: m(sess.TaxTotalMinor),
			Shipping: m(sess.ShippingMinor), Total: m(sess.TotalMinor), RequiresShipping: sess.RequiresShipping,
			PricedAt: sess.CreatedAt,
		})
	})
	return sess, err
}

func moneyIn(currency string) func(int64) events.Money {
	return func(minor int64) events.Money { return events.Money{AmountMinor: minor, Currency: currency} }
}

// quote builds a new session from the cart, the catalogue and pricing.
func (s *Service) quote(ctx context.Context, a authctx.AuthContext, cartType string) (domain.Session, error) {
	cart, err := s.p.Cart(ctx, a.ActorID, cartType)
	if err != nil {
		return domain.Session{}, startError("cart", err)
	}
	if !cart.Convertible {
		return domain.Session{}, ycerrors.Conflict("A " + cartType + " cart cannot be checked out")
	}
	if len(cart.Items) == 0 {
		return domain.Session{}, ycerrors.Conflict("The cart is empty")
	}
	ids := make([]string, len(cart.Items))
	for i, l := range cart.Items {
		ids[i] = l.SKUID
	}
	batch, err := s.p.SKUs(ctx, ids)
	if err != nil {
		return domain.Session{}, startError("catalogue", err)
	}
	skus := make(map[string]integration.SKU, len(batch.SKUs))
	for _, k := range batch.SKUs {
		skus[k.SKUID] = k
	}
	unavailable := slices.Clone(batch.Missing)
	for _, id := range ids {
		if k, ok := skus[id]; ok && k.Status != "active" {
			unavailable = append(unavailable, id)
		}
	}
	if len(unavailable) > 0 {
		return domain.Session{}, ycerrors.Conflict("Some items are no longer available").With("unavailable_skus", unavailable)
	}
	pricing, err := s.p.Evaluate(ctx, cart.Items)
	if err != nil {
		return domain.Session{}, startError("pricing", err)
	}
	if len(pricing.Unpriced) > 0 {
		return domain.Session{}, ycerrors.Conflict("Some items cannot be priced").With("unpriced", pricing.Unpriced)
	}
	priced := make(map[string]integration.PricedLine, len(pricing.Lines))
	for _, l := range pricing.Lines {
		priced[l.SKUID] = l
	}
	route, err := s.p.Route(ctx, pricing.StoreCurrency, "")
	if err != nil {
		return domain.Session{}, startError("payment", err)
	}

	now := s.now().UTC()
	sess := domain.Session{
		ID: uuid.NewString(), UserID: a.ActorID, UserType: domain.UserType(a.ActorType), CartType: cartType,
		CartID: cart.CartID, State: domain.StatePricing, Currency: pricing.StoreCurrency,
		TaxInclusive: pricing.TaxInclusive, TaxTotalMinor: pricing.Totals.TaxTotal.AmountMinor,
		PricingSnapshot: pricing.Raw, ExpiresAt: now.Add(s.cfg.SessionTTL), CreatedAt: now, UpdatedAt: now,
	}
	for _, l := range cart.Items {
		k, p := skus[l.SKUID], priced[l.SKUID]
		if p.Quantity != l.Quantity {
			return domain.Session{}, ycerrors.Internal("pricing did not price every line").
				WithInternal("sku %s: priced %d of %d", l.SKUID, p.Quantity, l.Quantity)
		}
		item := domain.Item{
			SKUID: l.SKUID, ProductID: k.ProductID, SKUCode: k.SKUCode, Name: k.ProductName,
			VariantValues: k.VariantValues, Quantity: l.Quantity, IsDigital: k.IsDigital,
			InventoryTracked: k.InventoryTracked, TaxClass: k.TaxClass, WeightGrams: k.WeightGrams,
			UnitPriceMinor: (p.Gross.AmountMinor + l.Quantity/2) / l.Quantity, LineTotalMinor: p.Gross.AmountMinor,
			TaxMinor: p.Tax.AmountMinor,
		}
		if k.Kind == "bundle" && len(k.Components) > 0 {
			item.Components = make([]domain.ComponentSnapshot, len(k.Components))
			for i, c := range k.Components {
				item.Components[i] = domain.ComponentSnapshot{
					SKUID: c.SKUID, Quantity: c.Quantity, ProductName: c.ProductName,
				}
			}
		}
		sess.Items = append(sess.Items, item)
		sess.ItemsTotalMinor += p.Gross.AmountMinor
		sess.RequiresShipping = sess.RequiresShipping || !k.IsDigital
		// Bundle SKUs are untracked; components drive RESERVING (composed-offers FR).
		sess.HasTrackedItems = sess.HasTrackedItems || k.InventoryTracked || len(item.Components) > 0
	}
	sess.TotalMinor = sess.ItemsTotalMinor
	sess.Route(routing(route))
	return sess, nil
}

// routing is how a session pays with the gateway: a gateway that cannot
// authorize separately is charged once (ADR-013).
func routing(r integration.Route) domain.Routing {
	v := domain.VariantAuthCapture
	if !r.SupportsSeparateAuthCapture {
		v = domain.VariantCharge
	}
	return domain.Routing{Gateway: r.Gateway, Variant: v, Hosted: r.HostedPayment, PublicKey: r.PublicKey}
}

func startError(participant string, err error) error {
	r, ok := integration.Rejected(err)
	if !ok {
		return err
	}
	if participant == "cart" && r.Status == http.StatusNotFound {
		return ycerrors.NotFound("cart type", "").Wrap(err)
	}
	return ycerrors.Conflict("Checkout cannot start: " + r.Problem.Detail).Wrap(err)
}

// Session returns one of the caller's sessions.
func (s *Service) Session(ctx context.Context, id string) (domain.Session, error) {
	var sess domain.Session
	err := s.asOwner(ctx, id, false, func(_ pgx.Tx, _ string, x *domain.Session) error {
		sess = *x
		return nil
	})
	return sess, err
}

// asOwner runs fn on one of the caller's sessions; forUpdate locks it.
func (s *Service) asOwner(ctx context.Context, id string, forUpdate bool, fn func(tx pgx.Tx, tenantID string, sess *domain.Session) error) error {
	a, err := requireActor(ctx, authctx.ActorCustomer, authctx.ActorVisitor)
	if err != nil {
		return err
	}
	return s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		sess, ok, err := s.repo.Session(ctx, tx, tenantID, id, forUpdate)
		if err != nil {
			return err
		}
		if !ok || !sess.Owns(a.ActorID, domain.UserType(a.ActorType)) {
			return ycerrors.NotFound("checkout session", id)
		}
		return fn(tx, tenantID, &sess)
	})
}

func (s *Service) requireQuote(sess domain.Session) error {
	switch {
	case sess.State == domain.StateExpired || (sess.State == domain.StatePricing && sess.Expired(s.now())):
		return ycerrors.Gone("The checkout session has expired; start a new checkout")
	case sess.State == domain.StateFailed:
		return ycerrors.Conflict("The checkout has ended; start a new checkout").With("state", string(sess.State))
	case sess.State != domain.StatePricing:
		return ycerrors.Conflict("The checkout has already been confirmed").With("state", string(sess.State))
	}
	return nil
}

// SelectShipping is the shipping step before confirm. A delivery address
// is quoted with yc-shipping and replaces any earlier one and its options;
// the named option, or the cheapest, is selected. Without an address the
// option is chosen from the current address's options. An address the
// shop does not ship to is 409 and leaves the session as it was.
// Customers may pass delivery_address_id from their book, or free-form
// JSON which is saved to the book (issue #32). Guests use JSON only.
func (s *Service) SelectShipping(ctx context.Context, id string, in domain.ShippingChoice) (domain.Session, error) {
	sess, err := s.Session(ctx, id)
	if err != nil {
		return sess, err
	}
	if err := s.requireQuote(sess); err != nil {
		return sess, err
	}
	if !sess.RequiresShipping {
		return sess, ycerrors.Validation().Field("delivery_address", "invalid", "Digital-only checkouts are not shipped").Build()
	}
	if in.DeliveryAddressID != "" && in.DeliveryAddress != nil {
		return sess, ycerrors.Validation().
			Field("delivery_address_id", "invalid", "Give either delivery_address_id or delivery_address, not both").
			Build()
	}

	addressID := in.DeliveryAddressID
	address := in.DeliveryAddress
	if addressID != "" {
		if sess.UserType != domain.UserCustomer {
			return sess, ycerrors.Validation().
				Field("delivery_address_id", "invalid", "Only customers can use a saved delivery address").
				Build()
		}
		book, err := s.p.GetDeliveryAddress(ctx, sess.UserID, addressID)
		if err != nil {
			if r, ok := integration.Rejected(err); ok && r.Status == http.StatusNotFound {
				return sess, ycerrors.NotFound("Address", addressID)
			}
			return sess, err
		}
		address = book.Address
	} else if address != nil && sess.UserType == domain.UserCustomer {
		created, err := s.p.CreateDeliveryAddress(ctx, sess.UserID, address)
		if err != nil {
			if r, ok := integration.Rejected(err); ok && r.Status == http.StatusBadRequest {
				return sess, ycerrors.Validation().Field("delivery_address", "invalid", r.Problem.Detail).Wrap(err)
			}
			return sess, err
		}
		addressID = created.ID
		address = created.Address
	}

	quoted := address != nil
	var options []domain.ShippingOption
	switch {
	case quoted:
		if v := domain.ValidateAddress(address); len(v) > 0 {
			return sess, validationError(v)
		}
		if options, err = s.quoteShipping(ctx, sess, address); err != nil {
			return sess, err
		}
	case in.OptionID == "":
		return sess, ycerrors.Validation().Field("delivery_address", "required", "Give a delivery address or a shipping option").Build()
	case len(sess.DeliveryAddress) == 0:
		return sess, ycerrors.Validation().Field("delivery_address", "required", "Give the delivery address to ship to first").Build()
	}

	var out domain.Session
	err = s.asOwner(ctx, id, true, func(tx pgx.Tx, tenantID string, sess *domain.Session) error {
		if err := s.requireQuote(*sess); err != nil {
			return err
		}
		ok := false
		if quoted {
			ok = sess.ShipTo(address, options, in.OptionID, addressID)
		} else {
			ok = sess.SelectShipping(in.OptionID)
		}
		if !ok {
			return ycerrors.Validation().Field("shipping_option_id", "invalid", "Choose one of the session's shipping options").Build()
		}
		now := s.now().UTC()
		sess.UpdatedAt = now
		if err := s.repo.UpdateSession(ctx, tx, tenantID, *sess); err != nil {
			return err
		}
		if err := s.history(ctx, tx, tenantID, sess.ID, "shipping_selected", "", "", map[string]any{
			"shipping_option_id": sess.ShippingOptionID, "options": len(sess.ShippingOptions), "address_quoted": quoted,
			"delivery_address_id": sess.DeliveryAddressID,
		}); err != nil {
			return err
		}
		out = *sess
		return s.publish(ctx, tx, tenantID, events.TypeCheckoutShippingSelectedV1, events.CheckoutShippingSelectedV1{
			SessionID: sess.ID, UserID: sess.UserID, ShippingOptionID: sess.ShippingOptionID,
			Amount: events.Money{AmountMinor: sess.ShippingMinor, Currency: sess.Currency}, SelectedAt: now,
		})
	})
	return out, err
}

// quoteShipping asks yc-shipping for the options to an address, weighing
// the physical lines. No options means the shop does not ship there.
func (s *Service) quoteShipping(ctx context.Context, sess domain.Session, address map[string]any) ([]domain.ShippingOption, error) {
	req := integration.ShippingQuoteRequest{DeliveryAddress: address, SubtotalMinor: sess.ItemsTotalMinor}
	for _, it := range sess.Items {
		req.Items = append(req.Items, integration.ShippingQuoteItem{
			SKUID: it.SKUID, Quantity: it.Quantity, WeightGrams: it.WeightGrams, IsDigital: it.IsDigital,
		})
	}
	q, err := s.p.QuoteShipping(ctx, req)
	if err != nil {
		if r, ok := integration.Rejected(err); ok {
			return nil, ycerrors.Conflict("Shipping cannot be quoted: " + r.Problem.Detail).Wrap(err)
		}
		return nil, err
	}
	if len(q.Options) == 0 {
		return nil, ycerrors.Conflict("The shop does not ship to this address")
	}
	out := make([]domain.ShippingOption, len(q.Options))
	for i, o := range q.Options {
		out[i] = domain.ShippingOption{
			ID: o.OptionID, RateID: o.RateID, Name: o.Name, AmountMinor: o.AmountMinor,
			EstimatedDaysMin: o.EstimatedDaysMin, EstimatedDaysMax: o.EstimatedDaysMax,
			ScheduleID: o.ScheduleID, RequiresDeliverySlot: o.RequiresDeliverySlot,
		}
	}
	return out, nil
}

// ListSlots returns available delivery windows for the selected shipping
// option's rate (issue #30). from/to are YYYY-MM-DD in the shop TZ; empty
// from defaults to today. The range is capped at MaxSlotListDays.
func (s *Service) ListSlots(ctx context.Context, id, from, to string) ([]domain.AvailableSlot, error) {
	sess, err := s.Session(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.requireQuote(sess); err != nil {
		return nil, err
	}
	opt, ok := sess.SelectedShipping()
	if !ok || !opt.RequiresDeliverySlot || opt.RateID == "" {
		return nil, ycerrors.Validation().Field("shipping_option_id", "invalid",
			"Select a shipping option that requires a delivery slot first").Build()
	}
	from, to, err = slotRange(from, to, s.now())
	if err != nil {
		return nil, err
	}
	list, err := s.p.ListSlots(ctx, opt.RateID, from, to)
	if err != nil {
		if r, ok := integration.Rejected(err); ok {
			return nil, ycerrors.Conflict("Delivery slots cannot be listed: " + r.Problem.Detail).Wrap(err)
		}
		return nil, err
	}
	out := make([]domain.AvailableSlot, len(list.Slots))
	for i, sl := range list.Slots {
		out[i] = domain.AvailableSlot{
			SlotID: sl.SlotID, StartAt: sl.StartAt.UTC(), EndAt: sl.EndAt.UTC(), Timezone: sl.Timezone,
			RemainingCapacity: sl.RemainingCapacity, Label: sl.Label,
		}
	}
	return out, nil
}

// SelectSlot soft-binds a delivery window on the session after verifying
// remaining capacity with yc-shipping. Capacity is not decremented until
// ORDER_CREATE books it.
func (s *Service) SelectSlot(ctx context.Context, id, slotID string) (domain.Session, error) {
	slotID = strings.TrimSpace(slotID)
	if slotID == "" {
		return domain.Session{}, ycerrors.Validation().Field("slot_id", "required", "A slot_id is required").Build()
	}
	sess, err := s.Session(ctx, id)
	if err != nil {
		return sess, err
	}
	if err := s.requireQuote(sess); err != nil {
		return sess, err
	}
	opt, ok := sess.SelectedShipping()
	if !ok || !opt.RequiresDeliverySlot {
		return sess, ycerrors.Validation().Field("shipping_option_id", "invalid",
			"Select a shipping option that requires a delivery slot first").Build()
	}
	sl, err := s.p.GetSlot(ctx, slotID)
	if err != nil {
		if r, ok := integration.Rejected(err); ok {
			if r.Status == http.StatusNotFound || r.Status == http.StatusConflict {
				return sess, ycerrors.Conflict("That delivery slot is no longer available; choose another").Wrap(err)
			}
			return sess, ycerrors.Validation().Field("slot_id", "invalid", r.Problem.Detail).Wrap(err)
		}
		return sess, err
	}
	if sl.RemainingCapacity < 1 {
		return sess, ycerrors.Conflict("That delivery slot is full; choose another")
	}
	bound := domain.NormaliseDeliverySlot(domain.DeliverySlot{
		SlotID: sl.SlotID, StartAt: sl.StartAt, EndAt: sl.EndAt, Timezone: sl.Timezone,
		Label: sl.Label, ScheduleID: firstNonEmpty(sl.ScheduleID, opt.ScheduleID),
	})
	var out domain.Session
	err = s.asOwner(ctx, id, true, func(tx pgx.Tx, tenantID string, sess *domain.Session) error {
		if err := s.requireQuote(*sess); err != nil {
			return err
		}
		opt, ok := sess.SelectedShipping()
		if !ok || !opt.RequiresDeliverySlot {
			return ycerrors.Validation().Field("shipping_option_id", "invalid",
				"Select a shipping option that requires a delivery slot first").Build()
		}
		sess.DeliverySlot = &bound
		sess.UpdatedAt = s.now().UTC()
		if err := s.repo.UpdateSession(ctx, tx, tenantID, *sess); err != nil {
			return err
		}
		if err := s.history(ctx, tx, tenantID, sess.ID, "slot_selected", "", "", map[string]any{
			"slot_id": bound.SlotID, "start_at": bound.StartAt, "end_at": bound.EndAt,
		}); err != nil {
			return err
		}
		out = *sess
		return nil
	})
	return out, err
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ensureSlotStillAvailable re-checks capacity at confirm (issue #30).
func (s *Service) ensureSlotStillAvailable(ctx context.Context, sess domain.Session) error {
	if !sess.NeedsDeliverySlot() || sess.DeliverySlot == nil {
		return nil
	}
	sl, err := s.p.GetSlot(ctx, sess.DeliverySlot.SlotID)
	if err != nil {
		if r, ok := integration.Rejected(err); ok {
			return ycerrors.Conflict("The chosen delivery slot is no longer available; choose another").Wrap(err).
				With("status", r.Status)
		}
		return err
	}
	if sl.RemainingCapacity < 1 {
		return ycerrors.Conflict("The chosen delivery slot is full; choose another")
	}
	return nil
}

// slotRange normalises YYYY-MM-DD from/to (default from = today UTC date)
// and enforces MaxSlotListDays.
func slotRange(from, to string, now time.Time) (string, string, error) {
	v := ycerrors.Validation()
	today := now.UTC().Format("2006-01-02")
	if from == "" {
		from = today
	}
	fromDay, err := time.Parse("2006-01-02", from)
	if err != nil {
		v.Field("from", "invalid", "from is YYYY-MM-DD")
	}
	if to == "" {
		to = fromDay.AddDate(0, 0, domain.MaxSlotListDays-1).Format("2006-01-02")
	}
	toDay, err := time.Parse("2006-01-02", to)
	if err != nil {
		v.Field("to", "invalid", "to is YYYY-MM-DD")
	}
	if !v.HasErrors() {
		if toDay.Before(fromDay) {
			v.Field("to", "invalid", "to must not be before from")
		} else if toDay.Sub(fromDay) > time.Duration(domain.MaxSlotListDays-1)*24*time.Hour {
			v.Field("to", "invalid", fmt.Sprintf("List at most %d days at a time", domain.MaxSlotListDays))
		}
	}
	if v.HasErrors() {
		return "", "", v.Build()
	}
	return from, to, nil
}

// Confirm validates the customer's details (ADR-010) and runs the saga
// inline until it finishes, waits on the customer or the deadline passes
// (ADR-019 rule 7). Repeating key resumes the saga, e.g. after 3-D
// Secure; another key for a confirmed session is a conflict.
func (s *Service) Confirm(ctx context.Context, id, key string, in domain.ConfirmInput) (domain.Session, error) {
	if key == "" || len(key) > 255 {
		return domain.Session{}, ycerrors.Validation().Field("Idempotency-Key", "required", "Idempotency-Key is required").Build()
	}
	route, routeErr := s.confirmRoute(ctx, id, in.PaymentGateway)
	if _, ok := integration.Rejected(routeErr); routeErr != nil && !ok {
		return domain.Session{}, routeErr
	}
	if pre, err := s.Session(ctx, id); err != nil {
		return domain.Session{}, err
	} else if pre.State == domain.StatePricing {
		if err := s.ensureSlotStillAvailable(ctx, pre); err != nil {
			return domain.Session{}, err
		}
	}
	var sess domain.Session
	var token string
	var rejected error
	err := s.asOwner(ctx, id, true, func(tx pgx.Tx, tenantID string, x *domain.Session) error {
		sess = *x
		if sess.State != domain.StatePricing {
			if sess.ConfirmKey != key {
				return s.requireQuote(sess)
			}
			return nil
		}
		if err := s.requireQuote(sess); err != nil {
			return err
		}
		if sess.RequiresShipping && in.DeliveryAddress != nil && len(sess.DeliveryAddress) > 0 &&
			sess.AddressDiffers(in.DeliveryAddress) {
			return ycerrors.Conflict("The delivery address differs from the one shipping was chosen for; choose shipping again").
				With("state", string(sess.State))
		}
		var violations []domain.Violation
		if routeErr != nil {
			violations = append(violations, domain.Violation{
				Field: "payment_gateway", Code: "invalid",
				Message: "The shop does not take payments in " + sess.Currency + " with this gateway",
			})
		} else if route != nil {
			sess.Route(*route)
		}
		violations = append(violations, sess.ValidateConfirm(in)...)
		if len(violations) > 0 {
			rejected = validationError(violations)
			return s.validationFailed(ctx, tx, tenantID, sess, violations)
		}
		now := s.now().UTC()
		token = s.newToken()
		until := now.Add(s.cfg.LeaseTTL)
		sess.Confirm(in, key, now)
		sess.LeaseOwner, sess.LeaseUntil = token, &until
		if err := s.repo.UpdateSession(ctx, tx, tenantID, sess); err != nil {
			return err
		}
		if err := s.history(ctx, tx, tenantID, sess.ID, "confirmed", domain.StatePricing, sess.State, nil); err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypeCheckoutConfirmedV1, events.CheckoutConfirmedV1{
			SessionID: sess.ID, UserID: sess.UserID, PaymentGateway: sess.PaymentGateway,
			Total: events.Money{AmountMinor: sess.TotalMinor, Currency: sess.Currency}, ConfirmedAt: now,
		})
	})
	if err != nil {
		return domain.Session{}, err
	}
	if rejected != nil {
		return domain.Session{}, rejected
	}
	if sess.State.Terminal() {
		return sess, nil
	}
	return s.runInline(ctx, sess, token)
}

// confirmRoute routes the gateway the customer names at confirm, or a
// session that started without one. nil keeps the session's gateway; a
// rejection means the shop does not take the payment with it.
func (s *Service) confirmRoute(ctx context.Context, id, gateway string) (*domain.Routing, error) {
	sess, err := s.Session(ctx, id)
	if err != nil || sess.State != domain.StatePricing || gateway == sess.PaymentGateway ||
		(gateway == "" && sess.PaymentGateway != "") {
		return nil, err
	}
	r, err := s.p.Route(ctx, sess.Currency, gateway)
	if err != nil {
		return nil, err
	}
	out := routing(r)
	return &out, nil
}

// runInline drives the session for the request, bounded by the confirm
// deadline. The saga outlives a client that disconnects.
func (s *Service) runInline(ctx context.Context, sess domain.Session, token string) (domain.Session, error) {
	a, _ := authctx.FromContext(ctx)
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.ConfirmDeadline)
	defer cancel()
	out, err := s.drive(runCtx, a.TenantID, sess.ID, token)
	if err != nil {
		telemetry.LoggerFromContext(ctx).Warn("checkout saga interrupted; the sweeper resumes it",
			"session_id", sess.ID, "err", err)
		if out.ID == "" {
			return s.load(context.WithoutCancel(ctx), a.TenantID, sess.ID)
		}
	}
	return out, nil
}

func (s *Service) validationFailed(ctx context.Context, tx pgx.Tx, tenantID string, sess domain.Session, violations []domain.Violation) error {
	fields := make([]string, len(violations))
	for i, v := range violations {
		fields[i] = v.Field
	}
	if err := s.history(ctx, tx, tenantID, sess.ID, "validation_failed", "", "", map[string]any{"fields": fields}); err != nil {
		return err
	}
	return s.publish(ctx, tx, tenantID, events.TypeCheckoutValidationFailedV1, events.CheckoutValidationFailedV1{
		SessionID: sess.ID, UserID: sess.UserID, Fields: fields, FailedAt: s.now().UTC(),
	})
}

func validationError(violations []domain.Violation) error {
	v := ycerrors.Validation()
	for _, x := range violations {
		v.Field(x.Field, x.Code, x.Message)
	}
	return v.Build()
}

// Cancel abandons the caller's session. A quote just ends; a payment
// waiting on the customer is compensated. Once the saga is running on
// its own it cannot be cancelled.
func (s *Service) Cancel(ctx context.Context, id string) (domain.Session, error) {
	var sess domain.Session
	var token string
	err := s.asOwner(ctx, id, true, func(tx pgx.Tx, tenantID string, x *domain.Session) error {
		sess = *x
		now := s.now().UTC()
		switch {
		case sess.State == domain.StateFailed || sess.State == domain.StateExpired:
			return nil
		case sess.State == domain.StatePricing:
			if err := sess.Compensate(domain.ReasonCancelled, nil, domain.StateFailed, now); err != nil {
				return err
			}
			sess.Finish(now)
			if err := s.repo.UpdateSession(ctx, tx, tenantID, sess); err != nil {
				return err
			}
			if err := s.history(ctx, tx, tenantID, sess.ID, "state_changed", domain.StatePricing, sess.State, nil); err != nil {
				return err
			}
			return s.ended(ctx, tx, tenantID, sess, domain.StatePricing)
		case sess.State == sess.PaymentState() && !sess.Leased(now) &&
			(sess.PaymentStatus == integration.PaymentPending || sess.PaymentStatus == integration.PaymentCreated):
			from := sess.State
			token = s.newToken()
			until := now.Add(s.cfg.LeaseTTL)
			sess.LeaseOwner, sess.LeaseUntil = token, &until
			if err := s.beginCompensation(ctx, tx, tenantID, &sess, domain.ReasonCancelled, nil, domain.StateFailed); err != nil {
				return err
			}
			if err := s.repo.UpdateSession(ctx, tx, tenantID, sess); err != nil {
				return err
			}
			return s.history(ctx, tx, tenantID, sess.ID, "state_changed", from, sess.State, nil)
		default:
			return ycerrors.Conflict("The checkout is being processed and cannot be cancelled").With("state", string(sess.State))
		}
	})
	if err != nil || token == "" {
		return sess, err
	}
	return s.runInline(ctx, sess, token)
}

// AdminSessions returns a page of the shop's sessions.
func (s *Service) AdminSessions(ctx context.Context, state domain.State, after *repository.SessionCursor, limit int) ([]domain.Session, error) {
	if _, err := requireActor(ctx, authctx.ActorShopAdmin); err != nil {
		return nil, err
	}
	var out []domain.Session
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		out, err = s.repo.ListSessions(ctx, tx, tenantID, state, after, limit)
		return err
	})
	return out, err
}

// AdminSession returns a session with its history for a shop admin.
func (s *Service) AdminSession(ctx context.Context, id string) (domain.Session, []repository.Event, error) {
	if _, err := requireActor(ctx, authctx.ActorShopAdmin); err != nil {
		return domain.Session{}, nil, err
	}
	return s.detail(ctx, id)
}

// InternalSession returns a session for a service caller.
func (s *Service) InternalSession(ctx context.Context, id string) (domain.Session, error) {
	sess, _, err := s.detail(ctx, id)
	return sess, err
}

func (s *Service) detail(ctx context.Context, id string) (domain.Session, []repository.Event, error) {
	var sess domain.Session
	var history []repository.Event
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		var ok bool
		var err error
		if sess, ok, err = s.repo.Session(ctx, tx, tenantID, id, false); err != nil {
			return err
		}
		if !ok {
			return ycerrors.NotFound("checkout session", id)
		}
		history, err = s.repo.Events(ctx, tx, tenantID, id)
		return err
	})
	return sess, history, err
}
