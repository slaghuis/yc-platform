package service

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	events "github.com/slaghuis/yc-platform/libs/yc-events"

	"github.com/slaghuis/yc-platform/services/yc-orders/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-orders/internal/integration"
	"github.com/slaghuis/yc-platform/services/yc-orders/internal/repository"
)

func newID() string { return uuid.NewString() }

// RequestReturn records the caller's request to return delivered items
// within the return window. An order has one request awaiting a decision
// at a time.
func (s *Service) RequestReturn(ctx context.Context, orderID string, items []domain.ReturnItem, reason string) (domain.Return, error) {
	if err := requireActor(ctx, authctx.ActorCustomer, authctx.ActorVisitor); err != nil {
		return domain.Return{}, err
	}
	visible := ownedBy(ctx)
	reason = strings.TrimSpace(reason)
	var r domain.Return
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		o, err := s.lock(ctx, tx, tenantID, orderID)
		if err != nil {
			return err
		}
		if !visible(o) {
			return ycerrors.NotFound("order", orderID)
		}
		now := s.now().UTC()
		if err := o.CheckReturnable(now); err != nil {
			return err
		}
		earlier, err := s.repo.Returns(ctx, tx, tenantID, o.ID)
		if err != nil {
			return err
		}
		for _, e := range earlier {
			if e.Status == domain.ReturnRequested {
				return ycerrors.Conflict("A return for this order is awaiting the shop's decision")
			}
		}
		if err := o.ValidateReturn(items, reason, earlier); err != nil {
			return err
		}
		r = domain.Return{
			ID: newID(), OrderID: o.ID, Status: domain.ReturnRequested, Items: items, Reason: reason,
			RequestedBy: actorID(ctx), RequestedAt: now,
		}
		if err := s.repo.InsertReturn(ctx, tx, tenantID, r); err != nil {
			if dbkit.IsUniqueViolation(err) {
				return ycerrors.Conflict("A return for this order is awaiting the shop's decision")
			}
			return err
		}
		evItems := make([]events.OrderReturnRequestedV1ItemsItem, len(items))
		for i, it := range items {
			evItems[i] = events.OrderReturnRequestedV1ItemsItem{SKUID: it.SKUID, Quantity: it.Quantity}
		}
		return s.publish(ctx, tx, tenantID, events.TypeOrderReturnRequestedV1, events.OrderReturnRequestedV1{
			OrderID: o.ID, OrderNumber: o.OrderNumber, UserID: o.UserID, ReturnID: r.ID, Items: evItems,
			Reason: optional(reason), RequestedAt: now,
		})
	})
	return r, err
}

// AcceptReturn accepts a return for a shop admin and refunds its lines
// through yc-payment at once, plus shipping when refundShipping is set.
// A return covering every physical line makes the order returned.
// Accepting again returns the return.
func (s *Service) AcceptReturn(ctx context.Context, id string, refundShipping bool, note string) (domain.Return, error) {
	var (
		r      domain.Return
		o      domain.Order
		amount int64
	)
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		if r, err = s.getReturn(ctx, tx, tenantID, id, false); err != nil || r.Status == domain.ReturnAccepted {
			return err
		}
		if r.Status != domain.ReturnRequested {
			return returnDecided(r)
		}
		if o, err = s.get(ctx, tx, tenantID, r.OrderID); err != nil {
			return err
		}
		earlier, err := s.repo.Returns(ctx, tx, tenantID, o.ID)
		if err != nil {
			return err
		}
		amount = o.ReturnRefund(r, refundShipping, earlier)
		return nil
	})
	if err != nil || r.Status == domain.ReturnAccepted {
		return r, err
	}

	key := "order:" + o.ID + ":return:" + r.ID
	var refund integration.Refund
	if amount > 0 && o.PaymentTransactionID != "" {
		if refund, err = s.payments.Refund(ctx, o.PaymentTransactionID, o.ID, amount, "return accepted", key); err != nil {
			return r, err
		}
	}

	raced := false
	err = s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		if o, err = s.lock(ctx, tx, tenantID, r.OrderID); err != nil {
			return err
		}
		if r, err = s.getReturn(ctx, tx, tenantID, id, true); err != nil || r.Status == domain.ReturnAccepted {
			return err
		}
		if r.Status != domain.ReturnRequested {
			// Rejected meanwhile: the money has moved, so the refund is
			// recorded all the same.
			raced = true
			if err := s.recordRefund(ctx, tx, tenantID, &o, refund, r.ID, "return accepted", key); err != nil {
				return err
			}
			return s.repo.UpdateOrder(ctx, tx, tenantID, o)
		}
		earlier, err := s.repo.Returns(ctx, tx, tenantID, o.ID)
		if err != nil {
			return err
		}
		now := s.now().UTC()
		r.Status, r.RefundShipping, r.RefundMinor = domain.ReturnAccepted, refundShipping && !domain.ShippingRefunded(earlier), refund.AmountMinor
		r.DecisionReason, r.DecidedBy, r.DecidedAt = truncate(strings.TrimSpace(note)), actorID(ctx), &now
		if err := s.repo.UpdateReturn(ctx, tx, tenantID, r); err != nil {
			return err
		}
		if err := s.recordRefund(ctx, tx, tenantID, &o, refund, r.ID, "return accepted", key); err != nil {
			return err
		}
		for i := range earlier {
			if earlier[i].ID == r.ID {
				earlier[i] = r
			}
		}
		if o.FullyReturned(earlier) && o.Status.CanBecome(domain.StatusReturned) {
			from := o.Status
			if err := o.Transition(domain.StatusReturned, now); err != nil {
				return err
			}
			if err := s.repo.AddHistory(ctx, tx, tenantID, o.ID, domain.HistoryEntry{
				From: from, To: o.Status, ActorID: actorID(ctx), Reason: "all items returned", CreatedAt: now,
			}); err != nil {
				return err
			}
		}
		if err := s.repo.UpdateOrder(ctx, tx, tenantID, o); err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypeOrderReturnAcceptedV1, events.OrderReturnAcceptedV1{
			OrderID: o.ID, OrderNumber: o.OrderNumber, UserID: o.UserID, ReturnID: r.ID,
			Refund: events.Money{AmountMinor: r.RefundMinor, Currency: o.Currency}, AcceptedAt: now,
		})
	})
	if err == nil && raced {
		err = returnDecided(r)
	}
	return r, err
}

// RejectReturn rejects a return for a shop admin, saying why. Rejecting
// again returns the return.
func (s *Service) RejectReturn(ctx context.Context, id, reason string) (domain.Return, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 500 {
		return domain.Return{}, ycerrors.Validation().
			Field("reason", "required", "Say why, in at most 500 characters").Build()
	}
	var r domain.Return
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		if r, err = s.getReturn(ctx, tx, tenantID, id, true); err != nil || r.Status == domain.ReturnRejected {
			return err
		}
		if r.Status != domain.ReturnRequested {
			return returnDecided(r)
		}
		o, err := s.get(ctx, tx, tenantID, r.OrderID)
		if err != nil {
			return err
		}
		now := s.now().UTC()
		r.Status, r.DecisionReason, r.DecidedBy, r.DecidedAt = domain.ReturnRejected, reason, actorID(ctx), &now
		if err := s.repo.UpdateReturn(ctx, tx, tenantID, r); err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypeOrderReturnRejectedV1, events.OrderReturnRejectedV1{
			OrderID: o.ID, OrderNumber: o.OrderNumber, UserID: o.UserID, ReturnID: r.ID, Reason: reason, RejectedAt: now,
		})
	})
	return r, err
}

// Returns returns a page of the shop admin's returns, newest first.
func (s *Service) Returns(ctx context.Context, status domain.ReturnStatus, after *repository.ReturnCursor, limit int) ([]domain.Return, error) {
	var out []domain.Return
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		out, err = s.repo.ListReturns(ctx, tx, tenantID, status, after, limit)
		return err
	})
	return out, err
}

func (s *Service) getReturn(ctx context.Context, tx pgx.Tx, tenantID, id string, forUpdate bool) (domain.Return, error) {
	r, ok, err := s.repo.Return(ctx, tx, tenantID, id, forUpdate)
	if err != nil {
		return r, err
	}
	if !ok {
		return r, ycerrors.NotFound("return", id)
	}
	return r, nil
}

func returnDecided(r domain.Return) error {
	return ycerrors.New(ycerrors.CodeInvalidStateTransition, "The return was already "+string(r.Status)).
		With("return_status", string(r.Status))
}
