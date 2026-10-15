package service

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	events "github.com/slaghuis/yc-platform/libs/yc-events"

	"github.com/slaghuis/yc-platform/services/yc-orders/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-orders/internal/integration"
)

// Package marks a received order packaged for a shop admin, booking its
// shipment with yc-shipping first (ADR-022). Packaging again returns the
// order.
func (s *Service) Package(ctx context.Context, id string) (domain.Order, error) {
	var o domain.Order
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		o, err = s.get(ctx, tx, tenantID, id)
		if err != nil || o.Status == domain.StatusPackaged {
			return err
		}
		probe := o
		return probe.Transition(domain.StatusPackaged, s.now())
	})
	if err != nil || o.Status == domain.StatusPackaged {
		return o, err
	}

	choice := o.ShippingChoice()
	sh, err := s.shipping.CreateShipment(ctx, integration.ShipmentRequest{
		OrderID: o.ID, OrderNumber: o.OrderNumber, Method: choice.Method, RateID: choice.RateID,
		DeliveryAddress: o.DeliveryAddress,
	}, "order:"+o.ID+":package")
	if err != nil {
		return o, err
	}

	err = s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		if o, err = s.lock(ctx, tx, tenantID, id); err != nil || o.Status == domain.StatusPackaged {
			return err
		}
		from := o.Status
		now := s.now().UTC()
		if err := o.Transition(domain.StatusPackaged, now); err != nil {
			return err
		}
		o.Shipment.ID, o.Shipment.TrackingNumber = sh.ShipmentID, sh.TrackingNumber
		if err := s.repo.UpdateOrder(ctx, tx, tenantID, o); err != nil {
			return err
		}
		if err := s.repo.AddHistory(ctx, tx, tenantID, o.ID, domain.HistoryEntry{
			From: from, To: o.Status, ActorID: actorID(ctx), Reason: "tracking " + sh.TrackingNumber, CreatedAt: now,
		}); err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypeOrderPackagedV1, events.OrderPackagedV1{
			OrderID: o.ID, OrderNumber: o.OrderNumber, UserID: o.UserID, ShipmentID: sh.ShipmentID,
			TrackingNumber: sh.TrackingNumber, PackagedAt: now,
		})
	})
	return o, err
}

// ShipmentProgress is a shipping event about an order's shipment.
type ShipmentProgress struct {
	OrderID          string
	ShipmentID       string
	TrackingNumber   string
	Status           domain.Status // shipped, delivered or delivery_exception
	CarrierName      string
	CarrierReference string
	Message          string
	OccurredAt       time.Time
}

// RecordShipment moves an order on as its shipment progresses (shipping
// in_transit, delivered and delivery_exception events). Events for
// unknown orders, other shipments, or moves the order cannot make (a
// stale or repeated event) are dropped.
func (s *Service) RecordShipment(ctx context.Context, tenantID string, p ShipmentProgress) error {
	return s.inTenant(ctx, tenantID, func(tx pgx.Tx, tenantID string) error {
		o, ok, err := s.repo.Order(ctx, tx, tenantID, p.OrderID, true)
		if err != nil || !ok || (o.Shipment.ID != "" && o.Shipment.ID != p.ShipmentID) {
			return err
		}
		if p.CarrierName != "" {
			o.Shipment.CarrierName = p.CarrierName
		}
		if p.CarrierReference != "" {
			o.Shipment.CarrierReference = p.CarrierReference
		}
		if o.Status == p.Status || !o.Status.CanBecome(p.Status) {
			if o.Status != p.Status {
				slog.InfoContext(ctx, "shipping event dropped", "order_id", o.ID, "order_status", o.Status,
					"shipment_status", p.Status)
			}
			return s.repo.UpdateOrder(ctx, tx, tenantID, o)
		}
		from := o.Status
		now := s.now().UTC()
		if err := o.Transition(p.Status, now); err != nil {
			return err
		}
		at := p.OccurredAt.UTC()
		if o.Shipment.ShippedAt != nil && p.Status == domain.StatusShipped {
			o.Shipment.ShippedAt = &at
		}
		if o.Shipment.DeliveredAt != nil && p.Status == domain.StatusDelivered {
			o.Shipment.DeliveredAt = &at
		}
		if o.Shipment.ID == "" {
			o.Shipment.ID, o.Shipment.TrackingNumber = p.ShipmentID, p.TrackingNumber
		}
		if err := s.repo.UpdateOrder(ctx, tx, tenantID, o); err != nil {
			return err
		}
		if err := s.repo.AddHistory(ctx, tx, tenantID, o.ID, domain.HistoryEntry{
			From: from, To: o.Status, ActorID: "yc-shipping", Reason: truncate(strings.TrimSpace(p.Message)), CreatedAt: now,
		}); err != nil {
			return err
		}
		if p.Status == domain.StatusShipped {
			return s.publish(ctx, tx, tenantID, events.TypeOrderShippedV1, events.OrderShippedV1{
				OrderID: o.ID, OrderNumber: o.OrderNumber, UserID: o.UserID, TrackingNumber: o.Shipment.TrackingNumber,
				CarrierName: optional(o.Shipment.CarrierName), CarrierReference: optional(o.Shipment.CarrierReference),
				ShippedAt: at,
			})
		}
		if p.Status == domain.StatusDelivered {
			items := make([]events.OrderDeliveredV1ItemsItem, len(o.Items))
			for i, it := range o.Items {
				items[i] = events.OrderDeliveredV1ItemsItem{SKUID: it.SKUID, ProductID: optional(it.ProductID), Quantity: it.Quantity}
			}
			return s.publish(ctx, tx, tenantID, events.TypeOrderDeliveredV1, events.OrderDeliveredV1{
				OrderID: o.ID, OrderNumber: o.OrderNumber, UserID: o.UserID, Items: items, DeliveredAt: at,
			})
		}
		return nil
	})
}

// Refund refunds part of a placed order for a shop admin (a goodwill
// refund). key is the request's Idempotency-Key: a retry refunds once.
func (s *Service) Refund(ctx context.Context, id string, amountMinor int64, reason, key string) (domain.Order, error) {
	if strings.TrimSpace(key) == "" {
		return domain.Order{}, ycerrors.Validation().Field("Idempotency-Key", "required", "Idempotency-Key is required").Build()
	}
	paymentKey := "order:" + id + ":refund:" + key
	var (
		o    domain.Order
		done bool
	)
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		if o, err = s.get(ctx, tx, tenantID, id); err != nil {
			return err
		}
		if _, done, err = s.repo.RefundByKey(ctx, tx, tenantID, paymentKey); err != nil || done {
			return err
		}
		return o.ValidateRefund(amountMinor, reason)
	})
	if err != nil || done {
		return o, err
	}

	reason = truncate(strings.TrimSpace(reason))
	refund, err := s.payments.Refund(ctx, o.PaymentTransactionID, o.ID, amountMinor, reason, paymentKey)
	if err != nil {
		return o, err
	}

	err = s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		if o, err = s.lock(ctx, tx, tenantID, id); err != nil {
			return err
		}
		if err := s.recordRefund(ctx, tx, tenantID, &o, refund, "", reason, paymentKey); err != nil {
			return err
		}
		return s.repo.UpdateOrder(ctx, tx, tenantID, o)
	})
	return o, err
}

// recordRefund records a refund yc-payment made, once per payment key,
// and publishes order.refunded.v1. o's refunded total grows; the caller
// saves o.
func (s *Service) recordRefund(ctx context.Context, tx pgx.Tx, tenantID string, o *domain.Order, refund integration.Refund, returnID, reason, key string) error {
	if refund.AmountMinor <= 0 || (refund.Status != "pending" && refund.Status != "completed") {
		return nil
	}
	if _, done, err := s.repo.RefundByKey(ctx, tx, tenantID, key); err != nil || done {
		return err
	}
	now := s.now().UTC()
	if err := s.repo.InsertRefund(ctx, tx, tenantID, o.ID, domain.Refund{
		ID: newID(), ReturnID: returnID, AmountMinor: refund.AmountMinor, Status: refund.Status, Reason: reason,
		PaymentKey: key, ActorID: actorID(ctx), CreatedAt: now,
	}); err != nil {
		return err
	}
	o.RefundedMinor += refund.AmountMinor
	o.UpdatedAt = now
	e := events.OrderRefundedV1{
		OrderID: o.ID, OrderNumber: o.OrderNumber, UserID: o.UserID,
		Amount:        events.Money{AmountMinor: refund.AmountMinor, Currency: o.Currency},
		TotalRefunded: events.Money{AmountMinor: o.RefundedMinor, Currency: o.Currency},
		ReturnID:      optional(returnID), Reason: reason, RefundedAt: now,
	}
	return s.publish(ctx, tx, tenantID, events.TypeOrderRefundedV1, e)
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
