// Package events holds yc-review's event subscribers.
package events

import (
	"context"
	"encoding/json"
	"fmt"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	ycevents "github.com/slaghuis/yc-platform/libs/yc-events"
)

// Handlers are the use cases the subscribers drive.
type Handlers interface {
	OrderDelivered(ctx context.Context, tenantID string, p ycevents.OrderDeliveredV1) error
	ForgetUser(ctx context.Context, tenantID string, e ycevents.IdentityUserDeletedV1) error
}

// Subscription pairs a durable consumer with its handler.
type Subscription struct {
	Opts    eventbus.SubscribeOpts
	Handler eventbus.Handler
}

// Subscriptions are yc-review's consumers.
func Subscriptions(h Handlers) []Subscription {
	gdprOpts, gdpr := ycevents.OnUserDeleted("yc-review-gdpr", func(ctx context.Context, tenantID string, e ycevents.IdentityUserDeletedV1) error {
		return poisonInvalid(h.ForgetUser(ctx, tenantID, e))
	})
	return []Subscription{
		{Opts: gdprOpts, Handler: gdpr},
		{
			Opts: eventbus.SubscribeOpts{
				Subject:     "yc.*." + ycevents.TypeOrderDeliveredV1,
				DurableName: "yc-review-order-delivered",
			},
			Handler: func(ctx context.Context, evt eventbus.ConsumedEvent) error {
				var p ycevents.OrderDeliveredV1
				if err := decode(evt, &p); err != nil {
					return err
				}
				return poisonInvalid(h.OrderDelivered(ctx, evt.Envelope.TenantID, p))
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
