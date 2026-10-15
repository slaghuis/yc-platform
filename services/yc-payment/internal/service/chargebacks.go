package service

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"

	"github.com/slaghuis/yc-platform/services/yc-payment/internal/gateway"
	"github.com/slaghuis/yc-platform/services/yc-payment/internal/repository"
)

// webhookDispute records a chargeback the gateway reports and announces
// it, so yc-orders marks the order disputed and yc-monitor alerts the
// shop. Disputes on payments yc-payment does not know are logged and
// dropped; the dispute itself is answered in the gateway's dashboard.
func (s *Service) webhookDispute(ctx context.Context, tx pgx.Tx, tenantID, gw string, ev gateway.WebhookEvent) error {
	t, ok, err := s.repo.TransactionByReference(ctx, tx, tenantID, gw, ev.PaymentReference)
	if err != nil {
		return err
	}
	if !ok {
		telemetry.LoggerFromContext(ctx).Warn("dispute for an unknown payment",
			"gateway", gw, "payment_reference", ev.PaymentReference, "dispute", ev.Dispute.Reference)
		return nil
	}
	currency := ev.Dispute.Currency
	if currency == "" {
		currency = t.Currency
	}
	c := repository.Chargeback{
		ID: uuid.NewString(), TransactionID: t.ID, OrderID: t.OrderID, Gateway: gw,
		GatewayReference: ev.Dispute.Reference, AmountMinor: ev.Dispute.AmountMinor, Currency: currency,
		Reason: ev.Dispute.Reason, ReceivedAt: s.now().UTC(),
	}
	first, err := s.repo.InsertChargeback(ctx, tx, tenantID, c)
	if err != nil || !first {
		return err
	}
	return s.bus.Publish(ctx, tx, eventbus.Event{
		EventType: events.TypePaymentChargebackReceivedV1, TenantID: tenantID,
		Actor: eventbus.Actor{Type: "service", ID: ServiceName},
		Payload: events.PaymentChargebackReceivedV1{
			ChargebackID: c.ID, TransactionID: t.ID, OrderID: optional(t.OrderID), Gateway: gw,
			GatewayReference: c.GatewayReference, Amount: money(c.AmountMinor, c.Currency),
			Reason: optional(c.Reason), ReceivedAt: c.ReceivedAt,
		},
	})
}
