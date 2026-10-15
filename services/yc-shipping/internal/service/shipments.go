package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	"github.com/slaghuis/yc-platform/services/yc-shipping/internal/carrier"
	"github.com/slaghuis/yc-platform/services/yc-shipping/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-shipping/internal/repository"
)

// MaxQuoteItems bounds a quote's lines, as an order's.
const MaxQuoteItems = 100

// QuoteItem is one cart line to ship.
type QuoteItem struct {
	SKUID     string
	Quantity  int64
	WeightG   *int64
	IsDigital bool
}

// QuoteRequest is checkout asking what shipping to an address costs.
type QuoteRequest struct {
	DeliveryAddress map[string]any
	Items           []QuoteItem
	SubtotalMinor   int64
}

// Quote is the zone that covers the address and its priced options,
// cheapest first. No zone means the shop does not ship there.
type Quote struct {
	Zone    *domain.Zone
	Options []domain.Option
}

func (r QuoteRequest) validate() error {
	v := ycerrors.Validation()
	if len(r.DeliveryAddress) == 0 {
		v.Field("delivery_address", "required", "A delivery address is required (ADR-015)")
	}
	if len(r.Items) == 0 || len(r.Items) > MaxQuoteItems {
		v.Field("items", "invalid", fmt.Sprintf("A quote has 1 to %d lines", MaxQuoteItems))
	}
	for i, it := range r.Items {
		if uuid.Validate(it.SKUID) != nil {
			v.Field(fmt.Sprintf("items[%d].sku_id", i), "invalid", "SKU ID must be a UUID")
		}
		if it.Quantity <= 0 {
			v.Field(fmt.Sprintf("items[%d].quantity", i), "invalid", "Quantity must be positive")
		}
		if it.WeightG != nil && *it.WeightG < 0 {
			v.Field(fmt.Sprintf("items[%d].weight_grams", i), "invalid", "Weight must not be negative")
		}
	}
	if r.SubtotalMinor < 0 {
		v.Field("subtotal_minor", "invalid", "Subtotal must not be negative")
	}
	if v.HasErrors() {
		return v.Build()
	}
	return nil
}

// parcel sums the physical lines' weight.
func (r QuoteRequest) parcel() domain.Parcel {
	p := domain.Parcel{WeightKnown: true, SubtotalMinor: r.SubtotalMinor}
	for _, it := range r.Items {
		switch {
		case it.IsDigital:
		case it.WeightG == nil:
			p.WeightKnown = false
		default:
			p.WeightG += *it.WeightG * it.Quantity
		}
	}
	return p
}

// Quote prices shipping to an address for checkout (internal).
func (s *Service) Quote(ctx context.Context, req QuoteRequest) (Quote, error) {
	if err := req.validate(); err != nil {
		return Quote{}, err
	}
	var out Quote
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		zones, err := s.repo.Zones(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		z, ok := domain.ResolveZone(zones, req.DeliveryAddress)
		if !ok {
			out.Options = []domain.Option{}
			return nil
		}
		rates, err := s.repo.Rates(ctx, tx, tenantID, z.ID)
		if err != nil {
			return err
		}
		out.Zone, out.Options = &z, domain.Options(rates, req.parcel())
		return nil
	})
	return out, err
}

// ShipmentRequest is yc-orders asking for a packaged order's shipment.
type ShipmentRequest struct {
	OrderID         string
	OrderNumber     string
	Method          string
	RateID          string
	DeliveryAddress map[string]any
}

func (r ShipmentRequest) validate() error {
	v := ycerrors.Validation()
	if uuid.Validate(r.OrderID) != nil {
		v.Field("order_id", "invalid", "Order ID must be a UUID")
	}
	if r.OrderNumber == "" || len(r.OrderNumber) > 64 {
		v.Field("order_number", "required", "Order number is required")
	}
	if r.Method == "" || len(r.Method) > 64 {
		v.Field("method", "required", "Shipping method is required")
	}
	if r.RateID != "" && uuid.Validate(r.RateID) != nil {
		v.Field("rate_id", "invalid", "Rate ID must be a UUID")
	}
	if len(r.DeliveryAddress) == 0 {
		v.Field("delivery_address", "required", "A delivery address is required (ADR-015)")
	}
	if v.HasErrors() {
		return v.Build()
	}
	return nil
}

// CreateShipment books an order's shipment with the rate's carrier driver
// (the manual driver when the rate is gone) and publishes
// shipping.label_created.v1. An order has one shipment: asking again
// returns it.
func (s *Service) CreateShipment(ctx context.Context, req ShipmentRequest) (domain.Shipment, error) {
	if err := req.validate(); err != nil {
		return domain.Shipment{}, err
	}
	var (
		sh     domain.Shipment
		exists bool
		driver carrier.Driver = carrier.Manual{}
	)
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		if sh, exists, err = s.repo.ShipmentByOrder(ctx, tx, tenantID, req.OrderID, false); err != nil || exists {
			return err
		}
		if req.RateID == "" {
			return nil
		}
		rate, ok, err := s.repo.Rate(ctx, tx, tenantID, req.RateID)
		if d := s.drivers[rate.CarrierDriver]; ok && d != nil {
			driver = d
		}
		return err
	})
	if err != nil || exists {
		return sh, err
	}

	now := s.now().UTC()
	sh = domain.Shipment{
		ID: uuid.NewString(), OrderID: req.OrderID, OrderNumber: req.OrderNumber, RateID: req.RateID,
		Method: req.Method, CarrierDriver: driver.Name(), Status: domain.StatusLabelCreated,
		DeliveryAddress: req.DeliveryAddress, CreatedAt: now, UpdatedAt: now,
	}
	label, err := driver.CreateLabel(ctx, sh)
	if err != nil {
		return domain.Shipment{}, err
	}
	sh.TrackingNumber, sh.LabelURL = label.TrackingNumber, label.LabelURL

	err = s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		if existing, ok, err := s.repo.ShipmentByOrder(ctx, tx, tenantID, req.OrderID, true); err != nil || ok {
			sh = existing
			return err
		}
		if err := s.repo.InsertShipment(ctx, tx, tenantID, sh); err != nil {
			return err
		}
		if err := s.repo.AddEvent(ctx, tx, tenantID, sh.ID, actorID(ctx), domain.Event{
			Status: sh.Status, Message: "Label created", OccurredAt: now, RecordedAt: now,
		}); err != nil {
			return err
		}
		e := events.ShippingLabelCreatedV1{
			ShipmentID: sh.ID, OrderID: sh.OrderID, CarrierDriver: sh.CarrierDriver,
			TrackingNumber: sh.TrackingNumber, CreatedAt: now,
		}
		if sh.LabelURL != "" {
			e.LabelURL = &sh.LabelURL
		}
		return s.publish(ctx, tx, tenantID, events.TypeShippingLabelCreatedV1, e)
	})
	return sh, err
}

// Shipments returns a page of the shop admin's shipments, newest first.
func (s *Service) Shipments(ctx context.Context, status domain.Status, after *repository.ShipmentCursor, limit int) ([]domain.Shipment, *repository.ShipmentCursor, error) {
	var out []domain.Shipment
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		out, err = s.repo.ListShipments(ctx, tx, tenantID, status, after, limit+1)
		return err
	})
	if err != nil || len(out) <= limit {
		return out, nil, err
	}
	out = out[:limit]
	last := out[limit-1]
	return out, &repository.ShipmentCursor{CreatedAt: last.CreatedAt, ID: last.ID}, nil
}

// Shipment returns one of the shop admin's shipments with its history.
func (s *Service) Shipment(ctx context.Context, id string) (domain.Shipment, []domain.Event, error) {
	var (
		sh  domain.Shipment
		evs []domain.Event
	)
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		if sh, err = s.shipment(ctx, tx, tenantID, id, false); err != nil {
			return err
		}
		evs, err = s.repo.Events(ctx, tx, tenantID, id)
		return err
	})
	return sh, evs, err
}

// Track returns a shipment by tracking number for anyone who has it: its
// status and history, without the address or order.
func (s *Service) Track(ctx context.Context, tracking string) (domain.Shipment, []domain.Event, error) {
	tracking = strings.ToUpper(strings.TrimSpace(tracking))
	if !domain.ValidTrackingNumber(tracking) {
		return domain.Shipment{}, nil, ycerrors.NotFound("shipment", tracking)
	}
	var (
		sh  domain.Shipment
		evs []domain.Event
	)
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		var (
			ok  bool
			err error
		)
		if sh, ok, err = s.repo.ShipmentByTracking(ctx, tx, tenantID, tracking); err != nil {
			return err
		}
		if !ok {
			return ycerrors.NotFound("shipment", tracking)
		}
		evs, err = s.repo.Events(ctx, tx, tenantID, sh.ID)
		return err
	})
	return sh, evs, err
}

// maxClockSkew bounds how far ahead of the clock an update may claim to
// have happened.
const maxClockSkew = 5 * time.Minute

// Update records a shop admin's report on a manual shipment (in transit,
// delivered or an exception) and publishes the matching shipping event.
func (s *Service) Update(ctx context.Context, id string, u domain.Update) (domain.Shipment, error) {
	if err := u.Validate(); err != nil {
		return domain.Shipment{}, err
	}
	now := s.now().UTC()
	if u.OccurredAt.IsZero() {
		u.OccurredAt = now
	} else if u.OccurredAt.After(now.Add(maxClockSkew)) {
		return domain.Shipment{}, ycerrors.Validation().Field("occurred_at", "invalid", "That is in the future").Build()
	}
	u.OccurredAt = u.OccurredAt.UTC()
	var sh domain.Shipment
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		if sh, err = s.shipment(ctx, tx, tenantID, id, true); err != nil {
			return err
		}
		if d := s.drivers[sh.CarrierDriver]; d == nil || !d.ManualUpdates() {
			return ycerrors.Conflict("The carrier reports this shipment's progress")
		}
		if err := sh.Apply(u, now); err != nil {
			return err
		}
		if err := s.repo.UpdateShipment(ctx, tx, tenantID, sh); err != nil {
			return err
		}
		if err := s.repo.AddEvent(ctx, tx, tenantID, sh.ID, actorID(ctx), domain.Event{
			Status: u.Status, Message: strings.TrimSpace(u.Message), OccurredAt: u.OccurredAt, RecordedAt: now,
		}); err != nil {
			return err
		}
		return s.publishStatus(ctx, tx, tenantID, sh, u)
	})
	return sh, err
}

func (s *Service) publishStatus(ctx context.Context, tx pgx.Tx, tenantID string, sh domain.Shipment, u domain.Update) error {
	msg := strings.TrimSpace(u.Message)
	switch u.Status {
	case domain.StatusInTransit:
		return s.publish(ctx, tx, tenantID, events.TypeShippingInTransitV1, events.ShippingInTransitV1{
			ShipmentID: sh.ID, OrderID: sh.OrderID, TrackingNumber: sh.TrackingNumber,
			CarrierName: optional(sh.CarrierName), CarrierReference: optional(sh.CarrierReference),
			Message: optional(msg), OccurredAt: u.OccurredAt,
		})
	case domain.StatusDelivered:
		return s.publish(ctx, tx, tenantID, events.TypeShippingDeliveredV1, events.ShippingDeliveredV1{
			ShipmentID: sh.ID, OrderID: sh.OrderID, TrackingNumber: sh.TrackingNumber,
			Message: optional(msg), OccurredAt: u.OccurredAt,
		})
	case domain.StatusException:
		return s.publish(ctx, tx, tenantID, events.TypeShippingDeliveryExceptionV1, events.ShippingDeliveryExceptionV1{
			ShipmentID: sh.ID, OrderID: sh.OrderID, TrackingNumber: sh.TrackingNumber,
			Message: msg, OccurredAt: u.OccurredAt,
		})
	case domain.StatusLabelCreated, domain.StatusCancelled:
	}
	return nil
}

// CancelForOrder cancels a cancelled order's shipment if it has not left
// the shop (order.cancelled.v1). No shipment, or one already cancelled, is
// nothing to do; yc-orders does not cancel orders that have shipped.
func (s *Service) CancelForOrder(ctx context.Context, tenantID, orderID, reason string) error {
	return s.inTenant(ctx, tenantID, func(tx pgx.Tx, tenantID string) error {
		sh, ok, err := s.repo.ShipmentByOrder(ctx, tx, tenantID, orderID, true)
		if err != nil || !ok || sh.Status == domain.StatusCancelled {
			return err
		}
		if !sh.Status.CanMove(domain.StatusCancelled) {
			return nil
		}
		if d := s.drivers[sh.CarrierDriver]; d != nil {
			if err := d.Cancel(ctx, sh); err != nil {
				return err
			}
		}
		now := s.now().UTC()
		sh.Status, sh.UpdatedAt = domain.StatusCancelled, now
		if err := s.repo.UpdateShipment(ctx, tx, tenantID, sh); err != nil {
			return err
		}
		if err := s.repo.AddEvent(ctx, tx, tenantID, sh.ID, ServiceName, domain.Event{
			Status: sh.Status, Message: "Order cancelled", OccurredAt: now, RecordedAt: now,
		}); err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypeShippingCancelledV1, events.ShippingCancelledV1{
			ShipmentID: sh.ID, OrderID: sh.OrderID, TrackingNumber: &sh.TrackingNumber,
			Reason: reasonOr(reason), CancelledAt: now,
		})
	})
}

func (s *Service) shipment(ctx context.Context, tx pgx.Tx, tenantID, id string, forUpdate bool) (domain.Shipment, error) {
	sh, ok, err := s.repo.Shipment(ctx, tx, tenantID, id, forUpdate)
	if err != nil {
		return sh, err
	}
	if !ok {
		return sh, ycerrors.NotFound("shipment", id)
	}
	return sh, nil
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func reasonOr(reason string) string {
	if reason = strings.TrimSpace(reason); reason == "" {
		return "order cancelled"
	}
	if len(reason) > 500 {
		return strings.ToValidUTF8(reason[:500], "")
	}
	return reason
}
