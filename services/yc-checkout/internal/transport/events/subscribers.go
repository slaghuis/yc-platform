// Package events holds yc-checkout's event subscribers: the saga's
// asynchronous inputs (ADR-019 rule 9).
package events

import (
	"context"
	"encoding/json"
	"fmt"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	ycevents "github.com/slaghuis/yc-platform/libs/yc-events"

	"github.com/slaghuis/yc-platform/services/yc-checkout/internal/domain"
)

// Handlers are the use cases the subscribers drive.
type Handlers interface {
	ReservationExpired(ctx context.Context, tenantID, sessionID, reservationID string) error
	PaymentSettled(ctx context.Context, tenantID, sessionID string, waitingIn domain.State) error
}

// Subscription pairs a durable consumer with its handler.
type Subscription struct {
	Opts    eventbus.SubscribeOpts
	Handler eventbus.Handler
}

// paymentEvent is the part of a payment event the saga needs.
type paymentEvent struct {
	SessionID string `json:"session_id"`
}

// Subscriptions are yc-checkout's consumers.
func Subscriptions(h Handlers) []Subscription {
	subs := make([]Subscription, 0, 5)
	subs = append(subs, Subscription{
		Opts: eventbus.SubscribeOpts{
			Subject:     "yc.*." + ycevents.TypeInventoryStockReservationExpiredV1,
			DurableName: "yc-checkout-reservation-expired",
		},
		Handler: func(ctx context.Context, evt eventbus.ConsumedEvent) error {
			var p ycevents.InventoryStockReservationExpiredV1
			if err := decode(evt, &p); err != nil {
				return err
			}
			return poisonInvalid(h.ReservationExpired(ctx, evt.Envelope.TenantID, p.SessionID, p.ReservationID))
		},
	})
	for _, w := range []struct {
		eventType, durable string
		state              domain.State
	}{
		{ycevents.TypePaymentAuthorizedV1, "yc-checkout-payment-authorized", domain.StateAuthorizing},
		{ycevents.TypePaymentAuthorizationFailedV1, "yc-checkout-payment-authorization-failed", domain.StateAuthorizing},
		{ycevents.TypePaymentCapturedV1, "yc-checkout-payment-captured", domain.StateCapturing},
		{ycevents.TypePaymentCaptureFailedV1, "yc-checkout-payment-capture-failed", domain.StateCapturing},
	} {
		subs = append(subs, Subscription{
			Opts: eventbus.SubscribeOpts{Subject: "yc.*." + w.eventType, DurableName: w.durable},
			Handler: func(ctx context.Context, evt eventbus.ConsumedEvent) error {
				var p paymentEvent
				if err := decode(evt, &p); err != nil {
					return err
				}
				return poisonInvalid(h.PaymentSettled(ctx, evt.Envelope.TenantID, p.SessionID, w.state))
			},
		})
	}
	return subs
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
