// Package events holds yc-product's event subscribers, which feed the
// search projection.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	ycevents "github.com/slaghuis/yc-platform/libs/yc-events"
)

// Handlers are the use cases the subscribers drive.
type Handlers interface {
	RecordCartAdd(ctx context.Context, tenantID, skuID string) error
	SetBasePrice(ctx context.Context, tenantID, skuID string, minor *int64, at time.Time) error
	SetInStock(ctx context.Context, tenantID, skuID string, inStock bool, at time.Time) error
}

// Subscription pairs a durable consumer with its handler.
type Subscription struct {
	Opts    eventbus.SubscribeOpts
	Handler eventbus.Handler
}

// Subscriptions are yc-product's consumers.
func Subscriptions(h Handlers) []Subscription {
	return []Subscription{
		{
			Opts: eventbus.SubscribeOpts{
				Subject:     "yc.*." + ycevents.TypeCartItemAddedV1,
				DurableName: "yc-product-cart-item-added",
			},
			Handler: func(ctx context.Context, evt eventbus.ConsumedEvent) error {
				var p ycevents.CartItemAddedV1
				if err := decode(evt, &p); err != nil {
					return err
				}
				return poisonInvalid(h.RecordCartAdd(ctx, evt.Envelope.TenantID, p.SKUID))
			},
		},
		{
			Opts: eventbus.SubscribeOpts{
				Subject:     "yc.*." + ycevents.TypePricingBasePriceSetV1,
				DurableName: "yc-product-base-price-set",
			},
			Handler: func(ctx context.Context, evt eventbus.ConsumedEvent) error {
				var p ycevents.PricingBasePriceSetV1
				if err := decode(evt, &p); err != nil {
					return err
				}
				minor := p.Price.AmountMinor
				return poisonInvalid(h.SetBasePrice(ctx, evt.Envelope.TenantID, p.SKUID, &minor, setAt(evt, p.ValidFrom)))
			},
		},
		{
			Opts: eventbus.SubscribeOpts{
				Subject:     "yc.*." + ycevents.TypePricingBasePriceRemovedV1,
				DurableName: "yc-product-base-price-removed",
			},
			Handler: func(ctx context.Context, evt eventbus.ConsumedEvent) error {
				var p ycevents.PricingBasePriceRemovedV1
				if err := decode(evt, &p); err != nil {
					return err
				}
				return poisonInvalid(h.SetBasePrice(ctx, evt.Envelope.TenantID, p.SKUID, nil, setAt(evt, p.RemovedAt)))
			},
		},
		{
			Opts: eventbus.SubscribeOpts{
				Subject:     "yc.*." + ycevents.TypeInventoryStockDepletedV1,
				DurableName: "yc-product-stock-depleted",
			},
			Handler: func(ctx context.Context, evt eventbus.ConsumedEvent) error {
				var p ycevents.InventoryStockDepletedV1
				if err := decode(evt, &p); err != nil {
					return err
				}
				return poisonInvalid(h.SetInStock(ctx, evt.Envelope.TenantID, p.SKUID, false, setAt(evt, p.DepletedAt)))
			},
		},
		{
			Opts: eventbus.SubscribeOpts{
				Subject:     "yc.*." + ycevents.TypeInventoryStockRestoredV1,
				DurableName: "yc-product-stock-restored",
			},
			Handler: func(ctx context.Context, evt eventbus.ConsumedEvent) error {
				var p ycevents.InventoryStockRestoredV1
				if err := decode(evt, &p); err != nil {
					return err
				}
				return poisonInvalid(h.SetInStock(ctx, evt.Envelope.TenantID, p.SKUID, true, setAt(evt, p.RestoredAt)))
			},
		},
	}
}

// setAt orders price and stock events: the payload's time, else when the
// event occurred.
func setAt(evt eventbus.ConsumedEvent, at time.Time) time.Time {
	if at.IsZero() {
		return evt.Envelope.EventTime
	}
	return at
}

func decode(evt eventbus.ConsumedEvent, out any) error {
	if err := json.Unmarshal(evt.Payload, out); err != nil {
		return fmt.Errorf("%w: decode payload: %w", eventbus.ErrPoison, err)
	}
	return nil
}

// poisonInvalid sends events with invalid content to the DLQ rather than
// retrying them.
func poisonInvalid(err error) error {
	if ycerrors.HasCode(err, ycerrors.CodeValidationFailed) {
		return fmt.Errorf("%w: %w", eventbus.ErrPoison, err)
	}
	return err
}
