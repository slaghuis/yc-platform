// Package events holds yc-preference's event subscribers.
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
	InitShop(ctx context.Context, tenantID, storeCurrency string) error
	SeedUser(ctx context.Context, tenantID, userID, email string) error
}

// Subscription pairs a durable consumer with its handler.
type Subscription struct {
	Opts    eventbus.SubscribeOpts
	Handler eventbus.Handler
}

// Subscriptions are yc-preference's consumers.
func Subscriptions(h Handlers) []Subscription {
	return []Subscription{
		{
			Opts: eventbus.SubscribeOpts{
				Subject:     "yc." + eventbus.PlatformTenantID + "." + ycevents.TypePlatformTenantProvisionedV1,
				DurableName: "yc-preference-tenant-provisioned",
			},
			Handler: func(ctx context.Context, evt eventbus.ConsumedEvent) error {
				var p ycevents.PlatformTenantProvisionedV1
				if err := decode(evt, &p); err != nil {
					return err
				}
				return poisonInvalid(h.InitShop(ctx, p.TenantID, p.StoreCurrency))
			},
		},
		{
			Opts: eventbus.SubscribeOpts{
				Subject:     "yc.*." + ycevents.TypeIdentityUserRegisteredV1,
				DurableName: "yc-preference-user-registered",
			},
			Handler: func(ctx context.Context, evt eventbus.ConsumedEvent) error {
				var p ycevents.IdentityUserRegisteredV1
				if err := decode(evt, &p); err != nil {
					return err
				}
				return poisonInvalid(h.SeedUser(ctx, evt.Envelope.TenantID, p.UserID, p.Email))
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
