package service

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"

	events "github.com/slaghuis/yc-platform/libs/yc-events"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	money "github.com/slaghuis/yc-platform/libs/yc-money"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	"github.com/slaghuis/yc-platform/services/yc-message/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-message/internal/integration"
)

// Welcome greets a new customer at the email they registered with.
func (s *Service) Welcome(ctx context.Context, tenantID, eventID string, p events.IdentityUserRegisteredV1) error {
	name := p.Email
	if p.DisplayName != nil && *p.DisplayName != "" {
		name = *p.DisplayName
	}
	to := domain.Recipient{UserID: p.UserID, Channel: domain.ChannelEmail, Address: p.Email}
	return s.notify(ctx, tenantID, eventID, domain.TypeWelcome, to, map[string]any{"name": name})
}

// OrderPlaced confirms a paid order to its customer or guest.
func (s *Service) OrderPlaced(ctx context.Context, tenantID, eventID string, p events.OrderPlacedV1) error {
	ctx = serviceContext(ctx, tenantID)
	var guest *integration.GuestContact
	if p.GuestContact != nil {
		guest = &integration.GuestContact{Email: p.GuestContact.Email, PhoneE164: p.GuestContact.PhoneE164}
	}
	to, ok, err := s.orderRecipient(ctx, p.UserID, p.UserType, guest)
	if err != nil || !ok {
		return s.skipped(ctx, err, domain.TypeOrderConfirmation, p.OrderID)
	}
	items := make([]map[string]any, len(p.Items))
	for i, it := range p.Items {
		items[i] = map[string]any{"name": it.NameSnapshot, "quantity": it.Quantity, "line_total": eventMoney(it.LineTotal)}
	}
	orderURL, err := s.orderURL(ctx, tenantID, p.OrderID, p.UserType)
	if err != nil {
		return err
	}
	return s.notify(ctx, tenantID, eventID, domain.TypeOrderConfirmation, to, map[string]any{
		"order_number": p.OrderNumber, "order_url": orderURL, "items": items,
		"items_total": eventMoney(p.ItemsTotal), "shipping": eventMoney(p.Shipping),
		"tax_total": eventMoney(p.TaxTotal), "total": eventMoney(p.Total), "requires_shipping": p.RequiresShipping,
	})
}

// OrderShipped tells the customer their order is on its way.
func (s *Service) OrderShipped(ctx context.Context, tenantID, eventID string, p events.OrderShippedV1) error {
	carrier := ""
	if p.CarrierName != nil {
		carrier = *p.CarrierName
	}
	return s.orderMessage(ctx, tenantID, eventID, domain.TypeOrderShipped, p.OrderID, map[string]any{
		"tracking_number": p.TrackingNumber, "carrier": carrier,
		"tracking_url": s.links(ctx, tenantID).Tracking(p.TrackingNumber),
	})
}

// OrderDelivered tells the customer their order arrived.
func (s *Service) OrderDelivered(ctx context.Context, tenantID, eventID string, p events.OrderDeliveredV1) error {
	return s.orderMessage(ctx, tenantID, eventID, domain.TypeOrderDelivered, p.OrderID, nil)
}

// OrderCancelled tells the customer their order was cancelled, and what
// was refunded.
func (s *Service) OrderCancelled(ctx context.Context, tenantID, eventID string, p events.OrderCancelledV1) error {
	refund := ""
	if p.Refund != nil && p.Refund.AmountMinor > 0 {
		refund = eventMoney(*p.Refund)
	}
	return s.orderMessage(ctx, tenantID, eventID, domain.TypeOrderCancelled, p.OrderID, map[string]any{
		"reason": p.Reason, "refund": refund,
	})
}

// OrderRefunded tells the customer about a refund on their order.
func (s *Service) OrderRefunded(ctx context.Context, tenantID, eventID string, p events.OrderRefundedV1) error {
	return s.orderMessage(ctx, tenantID, eventID, domain.TypeRefundIssued, p.OrderID, map[string]any{
		"amount": eventMoney(p.Amount), "total_refunded": eventMoney(p.TotalRefunded), "reason": p.Reason,
	})
}

// CartAbandoned reminds a customer of the items in their cart.
func (s *Service) CartAbandoned(ctx context.Context, tenantID, eventID string, p events.CartAbandonedV1) error {
	ctx = serviceContext(ctx, tenantID)
	to, ok, err := s.customer(ctx, p.UserID)
	if err != nil || !ok {
		return s.skipped(ctx, err, domain.TypeCartAbandoned, p.CartID)
	}
	return s.notify(ctx, tenantID, eventID, domain.TypeCartAbandoned, to, map[string]any{
		"item_count": p.ItemCount, "quantity": p.Quantity, "cart_url": s.links(ctx, tenantID).Cart(),
	})
}

// ReviewPrompt asks a customer to review what they bought.
func (s *Service) ReviewPrompt(ctx context.Context, tenantID, eventID string, p events.ReviewPromptRequestedV1) error {
	ctx = serviceContext(ctx, tenantID)
	to, ok, err := s.customer(ctx, p.UserID)
	if err != nil || !ok {
		return s.skipped(ctx, err, domain.TypeReviewPrompt, p.OrderID)
	}
	products := make([]map[string]any, len(p.Products))
	for i, pr := range p.Products {
		products[i] = map[string]any{
			"name": pr.Name, "review_url": s.links(ctx, tenantID).Review(pr.ProductID),
		}
	}
	return s.notify(ctx, tenantID, eventID, domain.TypeReviewPrompt, to, map[string]any{
		"order_number": p.OrderNumber, "products": products,
	})
}

// orderMessage sends a message about an order, reading the order from
// yc-orders for its number and its customer or guest.
func (s *Service) orderMessage(ctx context.Context, tenantID, eventID string, t domain.Type, orderID string, extra map[string]any) error {
	ctx = serviceContext(ctx, tenantID)
	o, err := s.upstream.Order(ctx, orderID)
	if httpkit.UpstreamStatus(err) == http.StatusNotFound {
		return s.skipped(ctx, nil, t, orderID)
	}
	if err != nil {
		return err
	}
	to, ok, err := s.orderRecipient(ctx, o.UserID, o.UserType, o.GuestContact)
	if err != nil || !ok {
		return s.skipped(ctx, err, t, orderID)
	}
	orderURL, err := s.orderURL(ctx, tenantID, orderID, o.UserType)
	if err != nil {
		return err
	}
	msgCtx := map[string]any{"order_number": o.OrderNumber, "order_url": orderURL}
	for k, v := range extra {
		msgCtx[k] = v
	}
	return s.notify(ctx, tenantID, eventID, t, to, msgCtx)
}

// orderRecipient is an order's customer, through their preferences, or
// its guest, through the order's contact.
func (s *Service) orderRecipient(ctx context.Context, userID, userType string, guest *integration.GuestContact) (domain.Recipient, bool, error) {
	if userType == "customer" {
		return s.customer(ctx, userID)
	}
	if guest == nil {
		return domain.Recipient{}, false, nil
	}
	r, ok := domain.GuestContact(guest.Email, guest.PhoneE164).Resolve(s.hasDriver)
	return r, ok, nil
}

// notify queues a message for an event, once per event and type.
func (s *Service) notify(ctx context.Context, tenantID, eventID string, t domain.Type, to domain.Recipient, msgCtx map[string]any) error {
	return s.inTenant(ctx, tenantID, func(tx pgx.Tx, tenantID string) error {
		_, err := s.queue(ctx, tx, tenantID, request{
			Type: t, Reference: "event:" + eventID + ":" + string(t), To: to, Context: msgCtx,
		})
		return err
	})
}

// skipped logs a message that could not be addressed. An upstream error
// is returned so the event is retried.
func (s *Service) skipped(ctx context.Context, err error, t domain.Type, subject string) error {
	if err != nil {
		return err
	}
	telemetry.LoggerFromContext(ctx).Warn("no address to send the message to; skipped",
		"message_type", string(t), "subject_id", subject)
	return nil
}

// orderURL is the storefront order page. Guest orders carry a signed
// guest_token so the email works on another device (issue #22).
func (s *Service) orderURL(ctx context.Context, tenantID, orderID, userType string) (string, error) {
	links := s.links(ctx, tenantID)
	if userType != "visitor" {
		return links.Order(orderID), nil
	}
	ga, err := s.upstream.GuestAccess(ctx, orderID)
	if err != nil {
		return "", err
	}
	return links.OrderWithToken(orderID, ga.Token), nil
}

func eventMoney(m events.Money) string {
	return money.New(m.AmountMinor, m.Currency).String()
}
