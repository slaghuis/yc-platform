package service

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	events "github.com/slaghuis/yc-platform/libs/yc-events"

	"github.com/slaghuis/yc-platform/services/yc-orders/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-orders/internal/repository"
)

// maxCustomerIDs bounds the user IDs CustomerOrders looks up at once.
const maxCustomerIDs = 10

// AddressInUse reports whether addressID is referenced by any Status.Open()
// order in the caller's tenant (issue #32).
func (s *Service) AddressInUse(ctx context.Context, addressID string) (inUse bool, orderIDs []string, err error) {
	err = s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		orderIDs, err = s.repo.OpenOrderIDsForAddress(ctx, tx, tenantID, addressID)
		return err
	})
	return len(orderIDs) > 0, orderIDs, err
}

// CustomerOrders returns the orders of any of userIDs (a user and its
// pseudonym) and how many are unfinished: in progress, or with a return
// awaiting a decision. yc-identity refuses a deletion while any are; for
// a service caller.
func (s *Service) CustomerOrders(ctx context.Context, userIDs []string) ([]repository.CustomerOrder, int, error) {
	if len(userIDs) == 0 || len(userIDs) > maxCustomerIDs {
		return nil, 0, ycerrors.Validation().Field("user_id", "invalid", "Give 1 to 10 user IDs").Build()
	}
	var out []repository.CustomerOrder
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		out, err = s.repo.CustomerOrders(ctx, tx, tenantID, userIDs)
		return err
	})
	open := 0
	for _, o := range out {
		if o.Status.Open() || o.OpenReturn {
			open++
		}
	}
	return out, open, err
}

// ForgetCustomer pseudonymises a deleted customer's orders (ADR-005) and
// reports yc-orders' part of the cascade. A repeated event changes
// nothing.
func (s *Service) ForgetCustomer(ctx context.Context, tenantID string, e events.IdentityUserDeletedV1) error {
	return s.inTenant(ctx, tenantID, func(tx pgx.Tx, tenantID string) error {
		now := s.now().UTC()
		first, err := s.repo.RecordDeletion(ctx, tx, tenantID, e.UserID, e.Pseudonym, now)
		if err != nil || !first {
			return err
		}
		n, err := s.repo.Pseudonymise(ctx, tx, tenantID, e.UserID, e.Pseudonym, now)
		if err != nil {
			return err
		}
		return s.bus.Publish(ctx, tx, events.GDPRCompleted(tenantID, ServiceName, e, events.GDPRPseudonymized, n, now))
	})
}

// pseudonymiseIfDeleted pseudonymises o when its customer was deleted
// while it was being paid for.
func (s *Service) pseudonymiseIfDeleted(ctx context.Context, tx pgx.Tx, tenantID string, o *domain.Order) error {
	p, deleted, err := s.repo.Pseudonym(ctx, tx, tenantID, o.UserID)
	if err != nil || !deleted {
		return err
	}
	if _, err := s.repo.Pseudonymise(ctx, tx, tenantID, o.UserID, p, s.now().UTC()); err != nil {
		return err
	}
	o.Pseudonymise(p)
	return nil
}

// Chargeback is a disputed payment (payment.chargeback_received.v1).
type Chargeback struct {
	ChargebackID  string
	TransactionID string
	OrderID       string
	AmountMinor   int64
	Currency      string
	Reason        string
}

// Dispute moves a chargeback's order to disputed and publishes
// order.disputed.v1 (ADR-025). Chargebacks for unknown or unplaced orders
// are dropped; a repeated one changes nothing.
func (s *Service) Dispute(ctx context.Context, tenantID string, c Chargeback) error {
	return s.inTenant(ctx, tenantID, func(tx pgx.Tx, tenantID string) error {
		var (
			o   domain.Order
			ok  bool
			err error
		)
		if c.OrderID != "" {
			o, ok, err = s.repo.Order(ctx, tx, tenantID, c.OrderID, true)
		} else {
			o, ok, err = s.repo.OrderByTransaction(ctx, tx, tenantID, c.TransactionID)
		}
		if err != nil {
			return err
		}
		if !ok || o.ChargebackID == c.ChargebackID || !o.Status.CanBecome(domain.StatusDisputed) {
			if !ok || o.ChargebackID != c.ChargebackID {
				slog.InfoContext(ctx, "chargeback dropped", "chargeback_id", c.ChargebackID, "order_id", c.OrderID,
					"found", ok, "order_status", o.Status)
			}
			return nil
		}
		from := o.Status
		now := s.now().UTC()
		if err := o.Transition(domain.StatusDisputed, now); err != nil {
			return err
		}
		o.ChargebackID = c.ChargebackID
		if err := s.repo.UpdateOrder(ctx, tx, tenantID, o); err != nil {
			return err
		}
		if err := s.repo.AddHistory(ctx, tx, tenantID, o.ID, domain.HistoryEntry{
			From: from, To: o.Status, ActorID: "yc-payment", Reason: truncate("chargeback: " + c.Reason), CreatedAt: now,
		}); err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypeOrderDisputedV1, events.OrderDisputedV1{
			OrderID: o.ID, OrderNumber: o.OrderNumber, UserID: o.UserID, ChargebackID: c.ChargebackID,
			Amount: events.Money{AmountMinor: c.AmountMinor, Currency: c.Currency}, Reason: optional(c.Reason),
			DisputedAt: now,
		})
	})
}
