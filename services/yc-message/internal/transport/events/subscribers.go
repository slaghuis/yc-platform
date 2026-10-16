// Package events holds yc-message's event subscribers: each turns an event
// into a queued message.
package events

import (
	"context"
	"encoding/json"
	"fmt"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	ycevents "github.com/slaghuis/yc-platform/libs/yc-events"
)

// Handlers are the use cases the subscribers drive. Each gets the event's
// tenant and ID, which makes the message it queues idempotent.
type Handlers interface {
	Welcome(ctx context.Context, tenantID, eventID string, p ycevents.IdentityUserRegisteredV1) error
	OrderPlaced(ctx context.Context, tenantID, eventID string, p ycevents.OrderPlacedV1) error
	OrderShipped(ctx context.Context, tenantID, eventID string, p ycevents.OrderShippedV1) error
	OrderDelivered(ctx context.Context, tenantID, eventID string, p ycevents.OrderDeliveredV1) error
	OrderCancelled(ctx context.Context, tenantID, eventID string, p ycevents.OrderCancelledV1) error
	OrderRefunded(ctx context.Context, tenantID, eventID string, p ycevents.OrderRefundedV1) error
	CartAbandoned(ctx context.Context, tenantID, eventID string, p ycevents.CartAbandonedV1) error
	ReviewPrompt(ctx context.Context, tenantID, eventID string, p ycevents.ReviewPromptRequestedV1) error
	ChannelAdded(ctx context.Context, tenantID, eventID string, p ycevents.PreferenceChannelAddedV1) error
	ShopAdminPromoted(ctx context.Context, tenantID, eventID string, p ycevents.IdentityUserPromotedV1) error
	InvitationAccepted(ctx context.Context, tenantID, eventID string, p ycevents.IdentityUserInvitationAcceptedV1) error
	UserDemoted(ctx context.Context, tenantID, eventID string, p ycevents.IdentityUserDemotedV1) error
	UserSuspended(ctx context.Context, tenantID, eventID string, p ycevents.IdentityUserSuspendedV1) error
	UserReactivated(ctx context.Context, tenantID, eventID string, p ycevents.IdentityUserReactivatedV1) error
	AdminAlert(ctx context.Context, tenantID, eventID string, p ycevents.MonitorAlertOpenedV1) error
	Milestone(ctx context.Context, tenantID, eventID string, p ycevents.AnalyticsMilestoneReachedV1) error
	TenantProvisioned(ctx context.Context, tenantID string, p ycevents.PlatformTenantProvisionedV1) error
	TenantUpdated(ctx context.Context, tenantID string, p ycevents.PlatformTenantUpdatedV1) error
	TenantDeleted(ctx context.Context, tenantID string, p ycevents.PlatformTenantDeletedV1) error
	ForgetUser(ctx context.Context, tenantID string, e ycevents.IdentityUserDeletedV1) error
}

// Subscription pairs a durable consumer with its handler.
type Subscription struct {
	Opts    eventbus.SubscribeOpts
	Handler eventbus.Handler
}

// Subscriptions are yc-message's consumers.
func Subscriptions(h Handlers) []Subscription {
	gdprOpts, gdpr := ycevents.OnUserDeleted("yc-message-gdpr", func(ctx context.Context, tenantID string, e ycevents.IdentityUserDeletedV1) error {
		return poisonInvalid(h.ForgetUser(ctx, tenantID, e))
	})
	return []Subscription{
		{Opts: gdprOpts, Handler: gdpr},
		subscribe(ycevents.TypeIdentityUserPromotedV1, "user-promoted", h.ShopAdminPromoted),
		subscribe(ycevents.TypeIdentityUserInvitationAcceptedV1, "invitation-accepted", h.InvitationAccepted),
		subscribe(ycevents.TypeIdentityUserDemotedV1, "user-demoted", h.UserDemoted),
		subscribe(ycevents.TypeIdentityUserSuspendedV1, "user-suspended", h.UserSuspended),
		subscribe(ycevents.TypeIdentityUserReactivatedV1, "user-reactivated", h.UserReactivated),
		subscribe(ycevents.TypeMonitorAlertOpenedV1, "monitor-alert-opened", h.AdminAlert),
		subscribe(ycevents.TypeAnalyticsMilestoneReachedV1, "analytics-milestone-reached", h.Milestone),
		subscribeTenant(ycevents.TypePlatformTenantProvisionedV1, "tenant-provisioned", h.TenantProvisioned),
		subscribeTenant(ycevents.TypePlatformTenantUpdatedV1, "tenant-updated", h.TenantUpdated),
		subscribeTenant(ycevents.TypePlatformTenantDeletedV1, "tenant-deleted", h.TenantDeleted),
		subscribe(ycevents.TypeIdentityUserRegisteredV1, "user-registered", h.Welcome),
		subscribe(ycevents.TypeOrderPlacedV1, "order-placed", h.OrderPlaced),
		subscribe(ycevents.TypeOrderShippedV1, "order-shipped", h.OrderShipped),
		subscribe(ycevents.TypeOrderDeliveredV1, "order-delivered", h.OrderDelivered),
		subscribe(ycevents.TypeOrderCancelledV1, "order-cancelled", h.OrderCancelled),
		subscribe(ycevents.TypeOrderRefundedV1, "order-refunded", h.OrderRefunded),
		subscribe(ycevents.TypeCartAbandonedV1, "cart-abandoned", h.CartAbandoned),
		subscribe(ycevents.TypeReviewPromptRequestedV1, "review-prompt-requested", h.ReviewPrompt),
		subscribe(ycevents.TypePreferenceChannelAddedV1, "preference-channel-added", h.ChannelAdded),
	}
}

func subscribe[T any](eventType, name string, fn func(ctx context.Context, tenantID, eventID string, p T) error) Subscription {
	return Subscription{
		Opts: eventbus.SubscribeOpts{Subject: "yc.*." + eventType, DurableName: "yc-message-" + name},
		Handler: func(ctx context.Context, evt eventbus.ConsumedEvent) error {
			var p T
			if err := json.Unmarshal(evt.Payload, &p); err != nil {
				return fmt.Errorf("%w: decode payload: %w", eventbus.ErrPoison, err)
			}
			return poisonInvalid(fn(ctx, evt.Envelope.TenantID, evt.Envelope.EventID, p))
		},
	}
}

// subscribeTenant handles platform tenant lifecycle events published on
// _platform; the shop ID is in the payload, not the envelope.
func subscribeTenant[T any](eventType, name string, fn func(ctx context.Context, tenantID string, p T) error) Subscription {
	return Subscription{
		Opts: eventbus.SubscribeOpts{Subject: "yc.*." + eventType, DurableName: "yc-message-" + name},
		Handler: func(ctx context.Context, evt eventbus.ConsumedEvent) error {
			var p T
			if err := json.Unmarshal(evt.Payload, &p); err != nil {
				return fmt.Errorf("%w: decode payload: %w", eventbus.ErrPoison, err)
			}
			return poisonInvalid(fn(ctx, evt.Envelope.TenantID, p))
		},
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
