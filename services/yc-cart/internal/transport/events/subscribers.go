// Package events holds yc-cart's event subscribers.
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
	MergeVisitor(ctx context.Context, tenantID, userID, visitorID string) error
	SetDisplayCurrency(ctx context.Context, tenantID, userID, currency string, at time.Time) error
	RemoveSKUs(ctx context.Context, tenantID string, skuIDs []string) error
}

// Subscription pairs a durable consumer with its handler.
type Subscription struct {
	Opts    eventbus.SubscribeOpts
	Handler eventbus.Handler
}

// Subscriptions are yc-cart's consumers.
func Subscriptions(h Handlers) []Subscription {
	return []Subscription{
		{
			Opts: eventbus.SubscribeOpts{
				Subject:     "yc.*." + ycevents.TypeIdentityUserLoggedInV1,
				DurableName: "yc-cart-user-logged-in",
			},
			Handler: func(ctx context.Context, evt eventbus.ConsumedEvent) error {
				var p ycevents.IdentityUserLoggedInV1
				if err := decode(evt, &p); err != nil {
					return err
				}
				if p.Role != "customer" || p.PreviousVisitorID == nil {
					return nil
				}
				return poisonInvalid(h.MergeVisitor(ctx, evt.Envelope.TenantID, p.UserID, *p.PreviousVisitorID))
			},
		},
		{
			Opts: eventbus.SubscribeOpts{
				Subject:     "yc.*." + ycevents.TypePreferenceCurrencyChangedV1,
				DurableName: "yc-cart-currency-changed",
			},
			Handler: func(ctx context.Context, evt eventbus.ConsumedEvent) error {
				var p ycevents.PreferenceCurrencyChangedV1
				if err := decode(evt, &p); err != nil {
					return err
				}
				return poisonInvalid(h.SetDisplayCurrency(ctx, evt.Envelope.TenantID, p.UserID, p.DisplayCurrency, p.ChangedAt))
			},
		},
		{
			Opts: eventbus.SubscribeOpts{
				Subject:     "yc.*." + ycevents.TypeProductSKUDeletedV1,
				DurableName: "yc-cart-sku-deleted",
			},
			Handler: func(ctx context.Context, evt eventbus.ConsumedEvent) error {
				var p ycevents.ProductSKUDeletedV1
				if err := decode(evt, &p); err != nil {
					return err
				}
				return poisonInvalid(h.RemoveSKUs(ctx, evt.Envelope.TenantID, []string{p.SKUID}))
			},
		},
		{
			Opts: eventbus.SubscribeOpts{
				Subject:     "yc.*." + ycevents.TypeProductDeletedV1,
				DurableName: "yc-cart-product-deleted",
			},
			Handler: func(ctx context.Context, evt eventbus.ConsumedEvent) error {
				var p ycevents.ProductDeletedV1
				if err := decode(evt, &p); err != nil {
					return err
				}
				if len(p.SKUIds) == 0 {
					return nil
				}
				return poisonInvalid(h.RemoveSKUs(ctx, evt.Envelope.TenantID, p.SKUIds))
			},
		},
	}
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
