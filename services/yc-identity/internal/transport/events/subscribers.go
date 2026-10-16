// Package events holds yc-identity's event subscribers.
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
	OnTenantProvisioned(ctx context.Context, p ycevents.PlatformTenantProvisionedV1, actor eventbus.Actor) error
	OnTenantUpdated(ctx context.Context, p ycevents.PlatformTenantUpdatedV1) error
	OnTenantDeleted(ctx context.Context, p ycevents.PlatformTenantDeletedV1) error
}

// Subscription pairs a durable consumer with its handler.
type Subscription struct {
	Opts    eventbus.SubscribeOpts
	Handler eventbus.Handler
}

// Subscriptions are yc-identity's consumers (ADR-030 rule 7).
func Subscriptions(h Handlers) []Subscription {
	return []Subscription{
		{
			Opts: eventbus.SubscribeOpts{
				Subject: "yc.*." + ycevents.TypePlatformTenantProvisionedV1, DurableName: "yc-identity-tenant-provisioned",
			},
			Handler: tenantProvisioned(h),
		},
		subscribeTenant(ycevents.TypePlatformTenantUpdatedV1, "tenant-updated", h.OnTenantUpdated),
		subscribeTenant(ycevents.TypePlatformTenantDeletedV1, "tenant-deleted", h.OnTenantDeleted),
	}
}

func tenantProvisioned(h Handlers) eventbus.Handler {
	return func(ctx context.Context, evt eventbus.ConsumedEvent) error {
		var p ycevents.PlatformTenantProvisionedV1
		if err := json.Unmarshal(evt.Payload, &p); err != nil {
			return fmt.Errorf("%w: decode payload: %w", eventbus.ErrPoison, err)
		}
		return poisonInvalid(h.OnTenantProvisioned(ctx, p, evt.Envelope.Actor))
	}
}

func subscribeTenant[T any](eventType, name string, fn func(ctx context.Context, p T) error) Subscription {
	return Subscription{
		Opts: eventbus.SubscribeOpts{Subject: "yc.*." + eventType, DurableName: "yc-identity-" + name},
		Handler: func(ctx context.Context, evt eventbus.ConsumedEvent) error {
			var p T
			if err := json.Unmarshal(evt.Payload, &p); err != nil {
				return fmt.Errorf("%w: decode payload: %w", eventbus.ErrPoison, err)
			}
			return poisonInvalid(fn(ctx, p))
		},
	}
}

func poisonInvalid(err error) error {
	if ycerrors.HasCode(err, ycerrors.CodeValidationFailed) {
		return fmt.Errorf("%w: %w", eventbus.ErrPoison, err)
	}
	return err
}
