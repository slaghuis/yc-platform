// Package service holds yc-orders' use cases. Every mutation writes its
// tables and publishes its events in one transaction.
package service

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"

	"github.com/slaghuis/yc-platform/services/yc-orders/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-orders/internal/integration"
	"github.com/slaghuis/yc-platform/services/yc-orders/internal/repository"
)

// ServiceName is the actor ID of events yc-orders publishes on its own
// behalf.
const ServiceName = "yc-orders"

// Publisher publishes through the outbox.
type Publisher interface {
	Publish(ctx context.Context, tx pgx.Tx, evt eventbus.Event) error
}

// Payments refunds captured payments.
type Payments interface {
	RefundAll(ctx context.Context, transactionID, orderID, reason, key string) (integration.Refund, error)
}

// Service implements the order use cases.
type Service struct {
	db       *pgxpool.Pool
	bus      Publisher
	payments Payments
	repo     repository.Orders
	now      func() time.Time
}

// New returns a Service.
func New(db *pgxpool.Pool, bus Publisher, payments Payments) *Service {
	return &Service{db: db, bus: bus, payments: payments, now: time.Now}
}

// Create records checkout's pending_payment order. A session has one
// order: creating it again returns it.
func (s *Service) Create(ctx context.Context, o domain.Order) (domain.Order, error) {
	if err := o.Validate(); err != nil {
		return domain.Order{}, err
	}
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		existing, ok, err := s.repo.OrderBySession(ctx, tx, tenantID, o.CheckoutSessionID)
		if err != nil {
			return err
		}
		if ok {
			o = existing
			return nil
		}
		now := s.now().UTC()
		o.ID, o.Status, o.CreatedAt, o.UpdatedAt = uuid.NewString(), domain.StatusPendingPayment, now, now
		if err := s.repo.InsertOrder(ctx, tx, tenantID, o); err != nil {
			if dbkit.IsUniqueViolation(err) {
				return ycerrors.Conflict("An order for this checkout session is being created")
			}
			return err
		}
		return s.repo.AddHistory(ctx, tx, tenantID, o.ID, domain.HistoryEntry{
			To: o.Status, ActorID: actorID(ctx), Reason: "checkout", CreatedAt: now,
		})
	})
	return o, err
}

// Confirm places a paid order: it gets its number and order.placed.v1 is
// published. Confirming a placed order returns it.
func (s *Service) Confirm(ctx context.Context, id, paymentTransactionID string) (domain.Order, error) {
	if paymentTransactionID != "" && uuid.Validate(paymentTransactionID) != nil {
		return domain.Order{}, ycerrors.Validation().
			Field("payment_transaction_id", "invalid", "Transaction ID must be a UUID").Build()
	}
	var o domain.Order
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		if o, err = s.lock(ctx, tx, tenantID, id); err != nil {
			return err
		}
		if o.Status == domain.StatusReceived {
			return nil
		}
		from := o.Status
		now := s.now().UTC()
		if err := o.Transition(domain.StatusReceived, now); err != nil {
			return err
		}
		if paymentTransactionID != "" {
			o.PaymentTransactionID = paymentTransactionID
		}
		n, err := s.repo.NextNumber(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		o.OrderNumber = domain.OrderNumber(tenantID, n)
		if err := s.repo.UpdateOrder(ctx, tx, tenantID, o); err != nil {
			return err
		}
		if err := s.repo.AddHistory(ctx, tx, tenantID, o.ID, domain.HistoryEntry{
			From: from, To: o.Status, ActorID: actorID(ctx), Reason: "payment captured", CreatedAt: now,
		}); err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypeOrderPlacedV1, placed(o))
	})
	return o, err
}

func placed(o domain.Order) events.OrderPlacedV1 {
	money := func(minor int64) events.Money { return events.Money{AmountMinor: minor, Currency: o.Currency} }
	items := make([]events.OrderPlacedV1ItemsItem, len(o.Items))
	for i, it := range o.Items {
		items[i] = events.OrderPlacedV1ItemsItem{
			SKUID: it.SKUID, SKUCode: it.SKUCode, NameSnapshot: it.NameSnapshot, Quantity: it.Quantity,
			UnitPrice: money(it.UnitPriceMinor), LineTotal: money(it.LineTotalMinor),
		}
	}
	e := events.OrderPlacedV1{
		OrderID: o.ID, OrderNumber: o.OrderNumber, UserID: o.UserID, UserType: string(o.UserType),
		CheckoutSessionID: o.CheckoutSessionID, Items: items, RequiresShipping: o.RequiresShipping,
		ItemsTotal: money(o.ItemsTotalMinor), Shipping: money(o.ShippingMinor), TaxTotal: money(o.TaxTotalMinor),
		Total: money(o.TotalMinor), PlacedAt: *o.PlacedAt,
	}
	if o.PaymentTransactionID != "" {
		e.PaymentTransactionID = &o.PaymentTransactionID
	}
	if g := o.GuestContact; g != nil {
		e.GuestContact = &events.GuestContact{Email: g.Email, PhoneE164: g.PhoneE164}
	}
	return e
}

// Abandon closes a pending order whose checkout was compensated. No
// order is nothing to undo: found is false. Abandoning again returns it.
func (s *Service) Abandon(ctx context.Context, id, reason string) (o domain.Order, found bool, err error) {
	err = s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		o, found, err = s.repo.Order(ctx, tx, tenantID, id, true)
		if err != nil || !found || o.Status == domain.StatusAbandoned {
			return err
		}
		from := o.Status
		now := s.now().UTC()
		if err := o.Transition(domain.StatusAbandoned, now); err != nil {
			return err
		}
		if err := s.repo.UpdateOrder(ctx, tx, tenantID, o); err != nil {
			return err
		}
		return s.repo.AddHistory(ctx, tx, tenantID, o.ID, domain.HistoryEntry{
			From: from, To: o.Status, ActorID: actorID(ctx), Reason: truncate(reason), CreatedAt: now,
		})
	})
	return o, found, err
}

// Cancel cancels a placed order for a shop admin, refunding its payment
// through yc-payment first. Restocking is a separate, manual step.
// Cancelling again returns the order.
func (s *Service) Cancel(ctx context.Context, id, reason string) (domain.Order, error) {
	var o domain.Order
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		o, err = s.get(ctx, tx, tenantID, id)
		if err != nil || o.Status == domain.StatusCancelled {
			return err
		}
		probe := o
		return probe.Transition(domain.StatusCancelled, s.now())
	})
	if err != nil || o.Status == domain.StatusCancelled {
		return o, err
	}

	var refund integration.Refund
	if o.PaymentTransactionID != "" && o.TotalMinor > 0 {
		refund, err = s.payments.RefundAll(ctx, o.PaymentTransactionID, o.ID, truncate(reason), "order:"+o.ID+":cancel")
		if err != nil {
			return o, err
		}
	}

	err = s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		if o, err = s.lock(ctx, tx, tenantID, id); err != nil || o.Status == domain.StatusCancelled {
			return err
		}
		from := o.Status
		now := s.now().UTC()
		if err := o.Transition(domain.StatusCancelled, now); err != nil {
			return err
		}
		o.RefundedMinor += refund.AmountMinor
		if err := s.repo.UpdateOrder(ctx, tx, tenantID, o); err != nil {
			return err
		}
		if err := s.repo.AddHistory(ctx, tx, tenantID, o.ID, domain.HistoryEntry{
			From: from, To: o.Status, ActorID: actorID(ctx), Reason: truncate(reason), CreatedAt: now,
		}); err != nil {
			return err
		}
		e := events.OrderCancelledV1{
			OrderID: o.ID, OrderNumber: o.OrderNumber, UserID: o.UserID, Reason: reasonOr(reason), CancelledAt: now,
		}
		if refund.AmountMinor > 0 {
			e.Refund = &events.Money{AmountMinor: refund.AmountMinor, Currency: o.Currency}
		}
		return s.publish(ctx, tx, tenantID, events.TypeOrderCancelledV1, e)
	})
	return o, err
}

// Order returns an order for a service caller, whatever its status.
func (s *Service) Order(ctx context.Context, id string) (domain.Order, []domain.HistoryEntry, error) {
	return s.detail(ctx, id, func(domain.Order) bool { return true })
}

// AdminOrder returns one of the shop admin's orders.
func (s *Service) AdminOrder(ctx context.Context, id string) (domain.Order, []domain.HistoryEntry, error) {
	if err := requireActor(ctx, authctx.ActorShopAdmin); err != nil {
		return domain.Order{}, nil, err
	}
	return s.detail(ctx, id, func(domain.Order) bool { return true })
}

// CustomerOrder returns one of the caller's placed orders. A visitor sees
// a guest order with the same visitor token (ADR-010).
func (s *Service) CustomerOrder(ctx context.Context, id string) (domain.Order, []domain.HistoryEntry, error) {
	if err := requireActor(ctx, authctx.ActorCustomer, authctx.ActorVisitor); err != nil {
		return domain.Order{}, nil, err
	}
	a, _ := authctx.FromContext(ctx)
	return s.detail(ctx, id, func(o domain.Order) bool { return o.UserID == a.ActorID && o.Status.Placed() })
}

func (s *Service) detail(ctx context.Context, id string, visible func(domain.Order) bool) (domain.Order, []domain.HistoryEntry, error) {
	var o domain.Order
	var history []domain.HistoryEntry
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		if o, err = s.get(ctx, tx, tenantID, id); err != nil {
			return err
		}
		if !visible(o) {
			return ycerrors.NotFound("order", id)
		}
		history, err = s.repo.History(ctx, tx, tenantID, id)
		return err
	})
	return o, history, err
}

// Mine returns a page of the calling customer's placed orders.
func (s *Service) Mine(ctx context.Context, after *repository.OrderCursor, limit int) ([]domain.Order, error) {
	if err := requireActor(ctx, authctx.ActorCustomer); err != nil {
		return nil, err
	}
	a, _ := authctx.FromContext(ctx)
	return s.list(ctx, repository.OrderFilter{UserID: a.ActorID}, after, limit)
}

// AdminList returns a page of the shop's orders. Without a status filter
// only placed orders are listed.
func (s *Service) AdminList(ctx context.Context, status domain.Status, after *repository.OrderCursor, limit int) ([]domain.Order, error) {
	if err := requireActor(ctx, authctx.ActorShopAdmin); err != nil {
		return nil, err
	}
	f := repository.OrderFilter{}
	if status != "" {
		f.Statuses = []domain.Status{status}
	}
	return s.list(ctx, f, after, limit)
}

func (s *Service) list(ctx context.Context, f repository.OrderFilter, after *repository.OrderCursor, limit int) ([]domain.Order, error) {
	var out []domain.Order
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		out, err = s.repo.ListOrders(ctx, tx, tenantID, f, after, limit)
		return err
	})
	return out, err
}

func (s *Service) get(ctx context.Context, tx pgx.Tx, tenantID, id string) (domain.Order, error) {
	o, ok, err := s.repo.Order(ctx, tx, tenantID, id, false)
	if err != nil {
		return o, err
	}
	if !ok {
		return o, ycerrors.NotFound("order", id)
	}
	return o, nil
}

func (s *Service) lock(ctx context.Context, tx pgx.Tx, tenantID, id string) (domain.Order, error) {
	o, ok, err := s.repo.Order(ctx, tx, tenantID, id, true)
	if err != nil {
		return o, err
	}
	if !ok {
		return o, ycerrors.NotFound("order", id)
	}
	return o, nil
}

func requireActor(ctx context.Context, types ...authctx.ActorType) error {
	a, _ := authctx.FromContext(ctx)
	for _, t := range types {
		if a.ActorType == t {
			return nil
		}
	}
	return ycerrors.Forbidden("Not permitted").WithInternal("actor_type %s", a.ActorType)
}

// asShopAdmin runs fn in the calling shop admin's shop.
func (s *Service) asShopAdmin(ctx context.Context, fn func(tx pgx.Tx, tenantID string) error) error {
	if err := requireActor(ctx, authctx.ActorShopAdmin); err != nil {
		return err
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

func actorID(ctx context.Context) string {
	a, _ := authctx.FromContext(ctx)
	return a.ActorID
}

// publish attributes the event to the caller, or to yc-orders itself.
func (s *Service) publish(ctx context.Context, tx pgx.Tx, tenantID, eventType string, payload any) error {
	actor := eventbus.Actor{Type: string(authctx.ActorService), ID: ServiceName}
	if a, ok := authctx.FromContext(ctx); ok && a.ActorType != "" {
		actor = eventbus.Actor{Type: string(a.ActorType), ID: a.ActorID, AccessTokenJTI: a.AccessTokenJTI}
	}
	return s.bus.Publish(ctx, tx, eventbus.Event{EventType: eventType, TenantID: tenantID, Actor: actor, Payload: payload})
}

// maxReason bounds free-text reasons.
const maxReason = 500

func truncate(s string) string {
	if len(s) > maxReason {
		return strings.ToValidUTF8(s[:maxReason], "")
	}
	return s
}

func reasonOr(reason string) string {
	if reason == "" {
		return "cancelled by the shop"
	}
	return truncate(reason)
}

func validateShopTenant(tenantID string) error {
	switch tenantID {
	case "":
		return ycerrors.Validation().Field("tenant_id", "required", "Tenant is required").Build()
	case eventbus.PlatformTenantID:
		return ycerrors.Validation().Field("tenant_id", "invalid", "Orders belong to a shop, not "+tenantID).Build()
	}
	if err := eventbus.ValidateTenantID(tenantID); err != nil {
		return ycerrors.Validation().Field("tenant_id", "invalid", "Tenant ID is not valid").Wrap(err)
	}
	return nil
}
