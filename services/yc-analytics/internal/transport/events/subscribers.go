// Package events holds yc-analytics' event subscribers.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	ycevents "github.com/slaghuis/yc-platform/libs/yc-events"
	"github.com/slaghuis/yc-platform/services/yc-analytics/internal/domain"
)

// Handlers are the use cases the subscribers drive.
type Handlers interface {
	TenantProvisioned(ctx context.Context, p ycevents.PlatformTenantProvisionedV1) error
	VisitorCreated(ctx context.Context, env eventbus.Envelope, p ycevents.IdentityVisitorCreatedV1) error
	CartEvent(ctx context.Context, env eventbus.Envelope, cartID, event, cartType, userType string, at time.Time) error
	CheckoutStep(ctx context.Context, env eventbus.Envelope, sessionID, state string, at time.Time) error
	Payment(ctx context.Context, env eventbus.Envelope, p domain.Payment) error
	OrderPlaced(ctx context.Context, env eventbus.Envelope, p ycevents.OrderPlacedV1) error
	OrderCancelled(ctx context.Context, env eventbus.Envelope, p ycevents.OrderCancelledV1) error
	OrderRefunded(ctx context.Context, env eventbus.Envelope, p ycevents.OrderRefundedV1) error
	OrderDelivered(ctx context.Context, env eventbus.Envelope, p ycevents.OrderDeliveredV1) error
	ForgetUser(ctx context.Context, tenantID string, e ycevents.IdentityUserDeletedV1) error
}

// Subscription pairs a durable consumer with its handler.
type Subscription struct {
	Opts    eventbus.SubscribeOpts
	Handler eventbus.Handler
}

// orderRetries gives events about an order time for its order.placed.v1,
// consumed separately, to be recorded first.
const orderRetries = 10

// Subscriptions are yc-analytics' consumers.
func Subscriptions(h Handlers) []Subscription {
	subs := []Subscription{
		sub(ycevents.TypePlatformTenantProvisionedV1, typed(func(ctx context.Context, _ eventbus.Envelope, p ycevents.PlatformTenantProvisionedV1) error {
			return h.TenantProvisioned(ctx, p)
		})),
		sub(ycevents.TypeIdentityVisitorCreatedV1, typed(h.VisitorCreated)),
		sub(ycevents.TypeCartCreatedV1, typed(func(ctx context.Context, env eventbus.Envelope, p ycevents.CartCreatedV1) error {
			return h.CartEvent(ctx, env, p.CartID, domain.CartCreated, p.CartType, p.UserType, p.CreatedAt)
		})),
		sub(ycevents.TypeCartAbandonedV1, typed(func(ctx context.Context, env eventbus.Envelope, p ycevents.CartAbandonedV1) error {
			return h.CartEvent(ctx, env, p.CartID, domain.CartAbandoned, p.CartType, "", p.AbandonedAt)
		})),
		sub(ycevents.TypeCartConvertedV1, typed(func(ctx context.Context, env eventbus.Envelope, p ycevents.CartConvertedV1) error {
			return h.CartEvent(ctx, env, p.CartID, domain.CartConverted, p.CartType, "", p.ConvertedAt)
		})),
		sub(ycevents.TypeCheckoutStartedV1, typed(func(ctx context.Context, env eventbus.Envelope, p ycevents.CheckoutStartedV1) error {
			return h.CheckoutStep(ctx, env, p.SessionID, domain.CheckoutStarted, p.StartedAt)
		})),
		sub(ycevents.TypeCheckoutConfirmedV1, typed(func(ctx context.Context, env eventbus.Envelope, p ycevents.CheckoutConfirmedV1) error {
			return h.CheckoutStep(ctx, env, p.SessionID, domain.CheckoutConfirmed, p.ConfirmedAt)
		})),
		sub(ycevents.TypeCheckoutCompletedV1, typed(func(ctx context.Context, env eventbus.Envelope, p ycevents.CheckoutCompletedV1) error {
			return h.CheckoutStep(ctx, env, p.SessionID, domain.CheckoutCompleted, p.CompletedAt)
		})),
		sub(ycevents.TypeCheckoutFailedV1, typed(func(ctx context.Context, env eventbus.Envelope, p ycevents.CheckoutFailedV1) error {
			return h.CheckoutStep(ctx, env, p.SessionID, domain.CheckoutFailed, p.FailedAt)
		})),
		sub(ycevents.TypeCheckoutExpiredV1, typed(func(ctx context.Context, env eventbus.Envelope, p ycevents.CheckoutExpiredV1) error {
			return h.CheckoutStep(ctx, env, p.SessionID, domain.CheckoutExpired, p.ExpiredAt)
		})),
		sub(ycevents.TypePaymentAuthorizedV1, typed(func(ctx context.Context, env eventbus.Envelope, p ycevents.PaymentAuthorizedV1) error {
			return h.Payment(ctx, env, payment(p.TransactionID, p.Gateway, p.Amount, domain.PaymentAuthorized, "", p.AuthorizedAt))
		})),
		sub(ycevents.TypePaymentCapturedV1, typed(func(ctx context.Context, env eventbus.Envelope, p ycevents.PaymentCapturedV1) error {
			return h.Payment(ctx, env, payment(p.TransactionID, p.Gateway, p.Amount, domain.PaymentSucceeded, "", p.CapturedAt))
		})),
		sub(ycevents.TypePaymentChargedV1, typed(func(ctx context.Context, env eventbus.Envelope, p ycevents.PaymentChargedV1) error {
			return h.Payment(ctx, env, payment(p.TransactionID, p.Gateway, p.Amount, domain.PaymentSucceeded, "", p.ChargedAt))
		})),
		sub(ycevents.TypePaymentAuthorizationFailedV1, typed(func(ctx context.Context, env eventbus.Envelope, p ycevents.PaymentAuthorizationFailedV1) error {
			return h.Payment(ctx, env, payment(p.TransactionID, p.Gateway, p.Amount, domain.PaymentFailed, p.Reason, p.FailedAt))
		})),
		sub(ycevents.TypePaymentCaptureFailedV1, typed(func(ctx context.Context, env eventbus.Envelope, p ycevents.PaymentCaptureFailedV1) error {
			return h.Payment(ctx, env, payment(p.TransactionID, p.Gateway, p.Amount, domain.PaymentFailed, p.Reason, p.FailedAt))
		})),
		sub(ycevents.TypePaymentChargeFailedV1, typed(func(ctx context.Context, env eventbus.Envelope, p ycevents.PaymentChargeFailedV1) error {
			return h.Payment(ctx, env, payment(p.TransactionID, p.Gateway, p.Amount, domain.PaymentFailed, p.Reason, p.FailedAt))
		})),
		sub(ycevents.TypeOrderPlacedV1, typed(h.OrderPlaced)),
	}
	for _, s := range []Subscription{
		sub(ycevents.TypeOrderCancelledV1, typed(h.OrderCancelled)),
		sub(ycevents.TypeOrderRefundedV1, typed(h.OrderRefunded)),
		sub(ycevents.TypeOrderDeliveredV1, typed(h.OrderDelivered)),
	} {
		s.Opts.MaxRetries = orderRetries
		subs = append(subs, s)
	}
	opts, handler := ycevents.OnUserDeleted("yc-analytics-gdpr", h.ForgetUser)
	return append(subs, Subscription{Opts: opts, Handler: handler})
}

func payment(txID, gateway string, amount ycevents.Money, outcome domain.PaymentOutcome, reason string, at time.Time) domain.Payment {
	return domain.Payment{
		TransactionID: txID, Gateway: gateway, Currency: amount.Currency, AmountMinor: amount.AmountMinor,
		Outcome: outcome, Reason: reason, At: at.UTC(),
	}
}

func sub(eventType string, h eventbus.Handler) Subscription {
	name := strings.NewReplacer(".", "-", "_", "-").Replace(strings.TrimSuffix(eventType, ".v1"))
	return Subscription{
		Opts:    eventbus.SubscribeOpts{Subject: "yc.*." + eventType, DurableName: "yc-analytics-" + name},
		Handler: h,
	}
}

func typed[T any](fn func(ctx context.Context, env eventbus.Envelope, p T) error) eventbus.Handler {
	return func(ctx context.Context, evt eventbus.ConsumedEvent) error {
		var p T
		if err := json.Unmarshal(evt.Payload, &p); err != nil {
			return fmt.Errorf("%w: decode payload: %w", eventbus.ErrPoison, err)
		}
		return poisonInvalid(fn(ctx, evt.Envelope, p))
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
