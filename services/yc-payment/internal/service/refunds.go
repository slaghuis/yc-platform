package service

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	events "github.com/slaghuis/yc-platform/libs/yc-events"

	"github.com/slaghuis/yc-platform/services/yc-payment/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-payment/internal/gateway"
)

// RefundRequest refunds part or all of a captured transaction.
type RefundRequest struct {
	TransactionID string
	// AmountMinor is the amount to refund; 0 refunds what is left.
	AmountMinor    int64
	Reason         string
	OrderID        string
	IdempotencyKey string
}

func (r RefundRequest) validate() error {
	v := ycerrors.Validation()
	if r.IdempotencyKey == "" || len(r.IdempotencyKey) > maxKey {
		v.Field("Idempotency-Key", "required", "Idempotency-Key header is required (at most 255 characters)")
	}
	if r.TransactionID == "" {
		v.Field("transaction_id", "required", "Transaction ID is required")
	}
	if r.AmountMinor < 0 {
		v.Field("amount_minor", "invalid", "Amount must not be negative")
	}
	if len(r.Reason) > domain.MaxReason {
		v.Field("reason", "too_long", "Reason is at most 500 characters")
	}
	if r.OrderID != "" && uuid.Validate(r.OrderID) != nil {
		v.Field("order_id", "invalid", "Order ID must be a UUID")
	}
	if v.HasErrors() {
		return v.Build()
	}
	return nil
}

// Refund returns money from a captured transaction. A repeated key returns
// the refund; one whose gateway call failed is sent again. found is false
// when there is no such transaction, which is nothing to undo.
func (s *Service) Refund(ctx context.Context, req RefundRequest) (r domain.Refund, found bool, err error) {
	if err := req.validate(); err != nil {
		return domain.Refund{}, false, err
	}
	var t domain.Transaction
	var creds gateway.Credentials
	var tenantID string
	err = s.inShop(ctx, func(tx pgx.Tx, tenant string) error {
		tenantID = tenant
		var err error
		existing, ok, err := s.repo.RefundByKey(ctx, tx, tenant, req.IdempotencyKey)
		if err != nil {
			return err
		}
		if ok && existing.TransactionID != req.TransactionID {
			return ycerrors.Conflict("Idempotency-Key was used for a different refund")
		}
		t, found, err = s.repo.Transaction(ctx, tx, tenant, req.TransactionID, true)
		if err != nil || !found {
			return err
		}
		if ok {
			r = existing
		} else if r, err = s.newRefund(ctx, tx, tenant, t, req); err != nil {
			return err
		}
		if r.Status == domain.RefundPending && r.GatewayReference == "" {
			creds, err = s.credentials(ctx, tx, tenant, t.Gateway, false)
		}
		return err
	})
	if err != nil || !found || r.Status != domain.RefundPending || r.GatewayReference != "" {
		return r, found, err
	}

	d, err := s.driver(t.Gateway)
	if err != nil {
		return r, true, err
	}
	g, err := d.Refund(ctx, creds, t.GatewayReference, r.AmountMinor, "yc-"+r.ID)
	var de *gateway.DeclineError
	switch {
	case errors.As(err, &de):
		g = gateway.Refund{Status: gateway.RefundFailed, FailureReason: de.Code}
	case err != nil:
		return r, true, err
	}
	err = s.inShop(ctx, func(tx pgx.Tx, _ string) error {
		cur, ok, err := s.repo.RefundByKey(ctx, tx, tenantID, req.IdempotencyKey)
		if err != nil || !ok {
			return errors.Join(err, ycerrors.NotFound("refund", r.ID))
		}
		r = cur
		return s.settleRefund(ctx, tx, tenantID, &r, g)
	})
	return r, true, err
}

func (s *Service) newRefund(ctx context.Context, tx pgx.Tx, tenantID string, t domain.Transaction, req RefundRequest) (domain.Refund, error) {
	pending, err := s.repo.PendingRefundTotal(ctx, tx, tenantID, t.ID)
	if err != nil {
		return domain.Refund{}, err
	}
	left := t.Refundable() - pending
	if t.Status != domain.StatusCaptured || left <= 0 {
		return domain.Refund{}, ycerrors.New(ycerrors.CodeInvalidStateTransition, "Nothing is left to refund").
			With("transaction_status", string(t.Status))
	}
	amount := req.AmountMinor
	if amount == 0 {
		amount = left
	}
	if amount > left {
		return domain.Refund{}, ycerrors.Validation().
			Field("amount_minor", "too_large", "Refund exceeds the refundable amount").Build()
	}
	a, _ := authctx.FromContext(ctx)
	actor := ""
	if a.ActorType == authctx.ActorShopAdmin {
		actor = a.ActorID
	}
	orderID := req.OrderID
	if orderID == "" {
		orderID = t.OrderID
	}
	now := s.now().UTC()
	r := domain.Refund{
		ID: uuid.NewString(), TransactionID: t.ID, OrderID: orderID, AmountMinor: amount, Reason: req.Reason,
		Status: domain.RefundPending, ActorID: actor, IdempotencyKey: req.IdempotencyKey, CreatedAt: now,
	}
	if err := s.repo.InsertRefund(ctx, tx, tenantID, r); err != nil {
		if dbkit.IsUniqueViolation(err) {
			return domain.Refund{}, ycerrors.Conflict("A refund with this Idempotency-Key is in progress")
		}
		return domain.Refund{}, err
	}
	return r, s.publish(ctx, tx, tenantID, events.TypePaymentRefundInitiatedV1, events.PaymentRefundInitiatedV1{
		RefundID: r.ID, TransactionID: t.ID, OrderID: optional(r.OrderID), Amount: money(amount, t.Currency),
		Reason: optional(r.Reason), InitiatedAt: now,
	})
}

// settleRefund records the gateway's answer for a refund; a completed
// refund counts against its transaction.
func (s *Service) settleRefund(ctx context.Context, tx pgx.Tx, tenantID string, r *domain.Refund, g gateway.Refund) error {
	if g.Reference != "" {
		r.GatewayReference = g.Reference
	}
	if r.Status != domain.RefundPending || g.Status == gateway.RefundPending {
		return s.repo.UpdateRefund(ctx, tx, tenantID, *r)
	}
	now := s.now().UTC()
	r.Status = domain.RefundStatus(g.Status)
	r.FailureReason = g.FailureReason
	r.CompletedAt = &now
	if err := s.repo.UpdateRefund(ctx, tx, tenantID, *r); err != nil {
		return err
	}
	t, ok, err := s.repo.Transaction(ctx, tx, tenantID, r.TransactionID, true)
	if err != nil || !ok {
		return errors.Join(err, ycerrors.NotFound("transaction", r.TransactionID))
	}
	amount := money(r.AmountMinor, t.Currency)
	if r.Status == domain.RefundFailed {
		return s.publish(ctx, tx, tenantID, events.TypePaymentRefundFailedV1, events.PaymentRefundFailedV1{
			RefundID: r.ID, TransactionID: t.ID, OrderID: optional(r.OrderID), Amount: amount,
			Reason: reasonOr(r.FailureReason), FailedAt: now,
		})
	}
	t.AddRefund(r.AmountMinor, now)
	if err := s.repo.UpdateTransaction(ctx, tx, tenantID, t); err != nil {
		return err
	}
	return s.publish(ctx, tx, tenantID, events.TypePaymentRefundCompletedV1, events.PaymentRefundCompletedV1{
		RefundID: r.ID, TransactionID: t.ID, OrderID: optional(r.OrderID), Amount: amount, CompletedAt: now,
	})
}

// webhookRefund applies a refund webhook. A refund the gateway names
// differently from its refund call's answer is matched by payment and
// amount. Refunds made outside yc-payment (the gateway's dashboard) are not
// tracked.
func (s *Service) webhookRefund(ctx context.Context, tx pgx.Tx, tenantID, gw string, ev gateway.WebhookEvent) error {
	r, ok, err := s.repo.RefundByReference(ctx, tx, tenantID, ev.Refund.Reference)
	if err != nil {
		return err
	}
	if !ok && ev.PaymentReference != "" && ev.RefundAmount > 0 {
		t, found, err := s.repo.TransactionByReference(ctx, tx, tenantID, gw, ev.PaymentReference)
		if err != nil || !found {
			return err
		}
		if r, ok, err = s.repo.PendingRefund(ctx, tx, tenantID, t.ID, ev.RefundAmount); err != nil {
			return err
		}
	}
	if !ok {
		return nil
	}
	return s.settleRefund(ctx, tx, tenantID, &r, ev.Refund)
}
