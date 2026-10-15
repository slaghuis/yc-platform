// Package events holds yc-orders' event subscribers: shipment progress
// from yc-shipping (ADR-022).
package events

import (
	"context"
	"encoding/json"
	"fmt"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	ycevents "github.com/slaghuis/yc-platform/libs/yc-events"

	"github.com/slaghuis/yc-platform/services/yc-orders/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-orders/internal/service"
)

// Handlers are the use cases the subscribers drive.
type Handlers interface {
	RecordShipment(ctx context.Context, tenantID string, p service.ShipmentProgress) error
}

// Subscription pairs a durable consumer with its handler.
type Subscription struct {
	Opts    eventbus.SubscribeOpts
	Handler eventbus.Handler
}

// Subscriptions are yc-orders' consumers.
func Subscriptions(h Handlers) []Subscription {
	on := func(eventType, durable string, progress func(raw json.RawMessage) (service.ShipmentProgress, error)) Subscription {
		return Subscription{
			Opts: eventbus.SubscribeOpts{Subject: "yc.*." + eventType, DurableName: durable},
			Handler: func(ctx context.Context, evt eventbus.ConsumedEvent) error {
				p, err := progress(evt.Payload)
				if err != nil {
					return fmt.Errorf("%w: decode payload: %w", eventbus.ErrPoison, err)
				}
				return poisonInvalid(h.RecordShipment(ctx, evt.Envelope.TenantID, p))
			},
		}
	}
	return []Subscription{
		on(ycevents.TypeShippingInTransitV1, "yc-orders-shipping-in-transit", func(raw json.RawMessage) (service.ShipmentProgress, error) {
			var e ycevents.ShippingInTransitV1
			err := json.Unmarshal(raw, &e)
			return service.ShipmentProgress{
				OrderID: e.OrderID, ShipmentID: e.ShipmentID, TrackingNumber: e.TrackingNumber, Status: domain.StatusShipped,
				CarrierName: deref(e.CarrierName), CarrierReference: deref(e.CarrierReference), Message: deref(e.Message),
				OccurredAt: e.OccurredAt,
			}, err
		}),
		on(ycevents.TypeShippingDeliveredV1, "yc-orders-shipping-delivered", func(raw json.RawMessage) (service.ShipmentProgress, error) {
			var e ycevents.ShippingDeliveredV1
			err := json.Unmarshal(raw, &e)
			return service.ShipmentProgress{
				OrderID: e.OrderID, ShipmentID: e.ShipmentID, TrackingNumber: e.TrackingNumber, Status: domain.StatusDelivered,
				Message: deref(e.Message), OccurredAt: e.OccurredAt,
			}, err
		}),
		on(ycevents.TypeShippingDeliveryExceptionV1, "yc-orders-shipping-delivery-exception", func(raw json.RawMessage) (service.ShipmentProgress, error) {
			var e ycevents.ShippingDeliveryExceptionV1
			err := json.Unmarshal(raw, &e)
			return service.ShipmentProgress{
				OrderID: e.OrderID, ShipmentID: e.ShipmentID, TrackingNumber: e.TrackingNumber,
				Status: domain.StatusDeliveryException, Message: e.Message, OccurredAt: e.OccurredAt,
			}, err
		}),
	}
}

// poisonInvalid sends events with invalid content to the DLQ rather than
// retrying them.
func poisonInvalid(err error) error {
	if ycerrors.HasCode(err, ycerrors.CodeValidationFailed) {
		return fmt.Errorf("%w: %w", eventbus.ErrPoison, err)
	}
	return err
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
