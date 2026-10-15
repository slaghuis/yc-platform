package service

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	ycevents "github.com/slaghuis/yc-platform/libs/yc-events"
	"github.com/slaghuis/yc-platform/services/yc-analytics/internal/domain"
)

// TenantProvisioned records a shop's name and store currency.
func (s *Service) TenantProvisioned(ctx context.Context, p ycevents.PlatformTenantProvisionedV1) error {
	return s.shopTx(ctx, p.TenantID, func(tx pgx.Tx) error {
		return s.facts.PutTenant(ctx, tx, domain.Tenant{
			TenantID: p.TenantID, Name: p.Name, StoreCurrency: p.StoreCurrency, ProvisionedAt: p.ProvisionedAt,
		}, s.now().UTC())
	})
}

// VisitorCreated records a visitor session, the top of the funnel.
func (s *Service) VisitorCreated(ctx context.Context, env eventbus.Envelope, p ycevents.IdentityVisitorCreatedV1) error {
	return s.fact(ctx, env.TenantID, p.CreatedAt, func(tx pgx.Tx) error {
		return s.facts.PutVisitor(ctx, tx, env.TenantID, env.EventID, p.CreatedAt)
	})
}

// CartEvent records a cart being created, abandoned or converted.
func (s *Service) CartEvent(ctx context.Context, env eventbus.Envelope, cartID, event, cartType, userType string, at time.Time) error {
	return s.fact(ctx, env.TenantID, at, func(tx pgx.Tx) error {
		return s.facts.PutCartEvent(ctx, tx, domain.CartEvent{
			CartID: cartID, TenantID: env.TenantID, Event: event, CartType: cartType,
			InFunnel: slices.Contains(s.cfg.FunnelCartTypes, cartType), UserType: userType, At: at,
		})
	})
}

// CheckoutStep records a checkout session reaching a state.
func (s *Service) CheckoutStep(ctx context.Context, env eventbus.Envelope, sessionID, state string, at time.Time) error {
	return s.fact(ctx, env.TenantID, at, func(tx pgx.Tx) error {
		return s.facts.PutCheckoutStep(ctx, tx, env.TenantID, sessionID, state, at)
	})
}

// Payment records a payment event on its transaction.
func (s *Service) Payment(ctx context.Context, env eventbus.Envelope, p domain.Payment) error {
	p.TenantID = env.TenantID
	return s.shopTx(ctx, env.TenantID, func(tx pgx.Tx) error {
		d, err := s.facts.PutPayment(ctx, tx, p)
		if err != nil {
			return err
		}
		return s.aggs.MarkDirty(ctx, tx, env.TenantID, dates(d, domain.Date(p.At)), s.now().UTC())
	})
}

// OrderPlaced records an order and announces the milestones it reached.
func (s *Service) OrderPlaced(ctx context.Context, env eventbus.Envelope, p ycevents.OrderPlacedV1) error {
	o := domain.Order{
		OrderID: p.OrderID, TenantID: env.TenantID, OrderNumber: p.OrderNumber,
		UserID: p.UserID, UserType: p.UserType, Currency: p.Total.Currency,
		ItemsMinor: p.ItemsTotal.AmountMinor, TaxMinor: p.TaxTotal.AmountMinor,
		ShippingMinor: p.Shipping.AmountMinor, TotalMinor: p.Total.AmountMinor,
		PlacedAt: p.PlacedAt.UTC(), Items: make([]domain.OrderItem, len(p.Items)),
	}
	if p.PaymentTransactionID != nil {
		o.TransactionID = *p.PaymentTransactionID
	}
	for i, it := range p.Items {
		o.Items[i] = domain.OrderItem{
			SKUID: it.SKUID, SKUCode: it.SKUCode, Name: it.NameSnapshot, Quantity: it.Quantity,
			UnitPriceMinor: it.UnitPrice.AmountMinor, LineTotalMinor: it.LineTotal.AmountMinor,
		}
	}
	return s.shopTx(ctx, env.TenantID, func(tx pgx.Tx) error {
		if err := s.facts.LockTenant(ctx, tx, env.TenantID); err != nil {
			return err
		}
		inserted, err := s.facts.InsertOrder(ctx, tx, o)
		if err != nil || !inserted {
			return err
		}
		if err := s.aggs.MarkDirty(ctx, tx, env.TenantID, dates(domain.Date(o.PlacedAt)), s.now().UTC()); err != nil {
			return err
		}
		return s.milestones(ctx, tx, o)
	})
}

// milestones announces the milestones o took its shop past, unless o is
// older than the rules' MaxAge (a replay of history).
func (s *Service) milestones(ctx context.Context, tx pgx.Tx, o domain.Order) error {
	now := s.now().UTC()
	if s.cfg.Milestones.MaxAge > 0 && now.Sub(o.PlacedAt) > s.cfg.Milestones.MaxAge {
		return nil
	}
	currency, ok, err := s.facts.StoreCurrency(ctx, tx, o.TenantID)
	if err != nil {
		return err
	}
	if !ok {
		currency = o.Currency
	}
	orders, revenue, err := s.facts.OrderTotals(ctx, tx, o.TenantID, currency)
	if err != nil {
		return err
	}
	beforeRevenue := revenue
	if o.Currency == currency {
		beforeRevenue -= o.TotalMinor
	}
	for _, m := range s.cfg.Milestones.Reached(orders-1, orders, beforeRevenue, revenue, currency) {
		inserted, err := s.facts.InsertMilestone(ctx, tx, o.TenantID, m, o.OrderID, now)
		if err != nil {
			return err
		}
		if !inserted {
			continue
		}
		p := ycevents.AnalyticsMilestoneReachedV1{
			Milestone: m.Kind, Threshold: m.Threshold, OrderID: &o.OrderID, ReachedAt: now,
		}
		if m.Currency != "" {
			p.Currency = &m.Currency
		}
		if err := s.publish(ctx, tx, o.TenantID, p); err != nil {
			return err
		}
	}
	return nil
}

// OrderCancelled marks an order cancelled.
func (s *Service) OrderCancelled(ctx context.Context, env eventbus.Envelope, p ycevents.OrderCancelledV1) error {
	return s.orderUpdate(ctx, env.TenantID, p.OrderID, func(tx pgx.Tx) (time.Time, bool, error) {
		return s.facts.CancelOrder(ctx, tx, p.OrderID, p.CancelledAt.UTC())
	})
}

// OrderRefunded records an order's total refunded.
func (s *Service) OrderRefunded(ctx context.Context, env eventbus.Envelope, p ycevents.OrderRefundedV1) error {
	return s.orderUpdate(ctx, env.TenantID, p.OrderID, func(tx pgx.Tx) (time.Time, bool, error) {
		return s.facts.RefundOrder(ctx, tx, p.OrderID, p.TotalRefunded.AmountMinor)
	})
}

// OrderDelivered records when an order was delivered.
func (s *Service) OrderDelivered(ctx context.Context, env eventbus.Envelope, p ycevents.OrderDeliveredV1) error {
	return s.orderUpdate(ctx, env.TenantID, p.OrderID, func(tx pgx.Tx) (time.Time, bool, error) {
		ok, err := s.facts.DeliverOrder(ctx, tx, p.OrderID, p.DeliveredAt.UTC())
		return time.Time{}, ok, err
	})
}

// orderUpdate applies a later order event. An order not recorded yet
// (its order.placed.v1 is consumed separately) is retried.
func (s *Service) orderUpdate(ctx context.Context, tenantID, orderID string, fn func(tx pgx.Tx) (time.Time, bool, error)) error {
	return s.shopTx(ctx, tenantID, func(tx pgx.Tx) error {
		d, ok, err := fn(tx)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("order %s is not recorded yet", orderID)
		}
		if d.IsZero() {
			return nil
		}
		return s.aggs.MarkDirty(ctx, tx, tenantID, dates(d), s.now().UTC())
	})
}

// ForgetUser replaces a deleted user's ID on their orders with their
// pseudonym (ADR-005) and reports it to the GDPR register.
func (s *Service) ForgetUser(ctx context.Context, tenantID string, e ycevents.IdentityUserDeletedV1) error {
	return s.shopTx(ctx, tenantID, func(tx pgx.Tx) error {
		n, err := s.facts.Pseudonymize(ctx, tx, tenantID, e.UserID, e.Pseudonym)
		if err != nil {
			return err
		}
		return s.bus.Publish(ctx, tx, ycevents.GDPRCompleted(tenantID, ServiceName, e,
			ycevents.GDPRPseudonymized, n, s.now().UTC()))
	})
}

// fact records a fact dated at and marks its day dirty.
func (s *Service) fact(ctx context.Context, tenantID string, at time.Time, fn func(tx pgx.Tx) error) error {
	return s.shopTx(ctx, tenantID, func(tx pgx.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		return s.aggs.MarkDirty(ctx, tx, tenantID, dates(domain.Date(at)), s.now().UTC())
	})
}

// dates removes duplicates.
func dates(ds ...time.Time) []time.Time {
	out := make([]time.Time, 0, len(ds))
	for _, d := range ds {
		if !slices.ContainsFunc(out, d.Equal) {
			out = append(out, d)
		}
	}
	return out
}
