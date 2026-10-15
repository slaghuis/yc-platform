package service

import (
	"context"

	"github.com/jackc/pgx/v5"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
)

// Orders finds a customer's orders (yc-orders).
type Orders interface {
	// CustomerOrderIDs returns the orders placed by any of userIDs in the
	// tenant of ctx.
	CustomerOrderIDs(ctx context.Context, userIDs ...string) ([]string, error)
}

// ForgetUser clears the delivery addresses of a deleted customer's
// shipments (ADR-005) and reports yc-shipping's part of the cascade.
// Shipments do not record the customer, so yc-orders names the orders; it
// is asked for the pseudonym too, in case it pseudonymised them first.
func (s *Service) ForgetUser(ctx context.Context, tenantID string, e events.IdentityUserDeletedV1) error {
	if err := validateShopTenant(tenantID); err != nil {
		return err
	}
	a, _ := authctx.FromContext(ctx)
	ctx = authctx.WithAuth(ctx, authctx.AuthContext{
		TenantID: tenantID, ActorType: authctx.ActorService, ActorID: ServiceName, CorrelationID: a.CorrelationID,
	})
	orderIDs, err := s.orders.CustomerOrderIDs(ctx, e.UserID, e.Pseudonym)
	if err != nil {
		return err
	}
	return s.inTenant(ctx, tenantID, func(tx pgx.Tx, tenantID string) error {
		now := s.now().UTC()
		var n int64
		if len(orderIDs) > 0 {
			if n, err = s.repo.ClearAddresses(ctx, tx, tenantID, orderIDs, now); err != nil {
				return err
			}
		}
		return s.bus.Publish(ctx, tx, events.GDPRCompleted(tenantID, ServiceName, e, events.GDPRAnonymized, n, now))
	})
}
