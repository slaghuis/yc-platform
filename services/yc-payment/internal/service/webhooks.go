package service

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"

	"github.com/slaghuis/yc-platform/services/yc-payment/internal/gateway"
)

// Webhook verifies and applies a gateway's webhook for the shop whose
// host it arrived on. Redeliveries are acknowledged without effect, as
// are events about payments yc-payment does not know. A bad signature is
// unauthorized and announced as a security event.
func (s *Service) Webhook(ctx context.Context, gw string, header http.Header, body []byte) error {
	d, err := s.driver(gw)
	if err != nil {
		return err
	}
	var secret string
	err = s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		creds, err := s.credentials(ctx, tx, tenantID, gw, false)
		secret = creds.WebhookSecret
		return err
	})
	if err != nil {
		return err
	}
	ev, err := d.ParseWebhook(secret, header, body, s.now())
	if errors.Is(err, gateway.ErrSignature) {
		telemetry.LoggerFromContext(ctx).Warn("webhook signature rejected", "gateway", gw, "err", err)
		if perr := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
			return s.publish(ctx, tx, tenantID, events.TypePaymentWebhookSignatureFailedV1, events.PaymentWebhookSignatureFailedV1{
				Gateway: gw, Reason: signatureReason(secret), ReceivedAt: s.now().UTC(),
			})
		}); perr != nil {
			return perr
		}
		return ycerrors.Unauthorized("Webhook signature is not valid")
	}
	if err != nil {
		return ycerrors.Validation().Message("Webhook is not valid").Wrap(err)
	}

	return s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		first, err := s.repo.RecordWebhook(ctx, tx, tenantID, gw, ev.ID, ev.Type, ev.Raw, s.now().UTC())
		if err != nil || !first {
			return err
		}
		switch ev.Kind {
		case gateway.WebhookPayment:
			t, ok, err := s.repo.TransactionByReference(ctx, tx, tenantID, gw, ev.PaymentReference)
			if err != nil || !ok {
				return err
			}
			return s.apply(ctx, tx, tenantID, &t, ev.Payment, "")
		case gateway.WebhookRefund:
			return s.webhookRefund(ctx, tx, tenantID, gw, ev)
		case gateway.WebhookOther:
		}
		return nil
	})
}

func signatureReason(secret string) string {
	if secret == "" {
		return "webhook_secret_not_configured"
	}
	return "signature_invalid"
}
