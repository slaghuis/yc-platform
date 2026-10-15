package service

import (
	"context"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
)

// ForgetUser pseudonymises a deleted user on the refunds they issued as a
// shop admin (ADR-005) and reports yc-payment's part of the cascade.
// Transactions record sessions and orders, never the customer, and are
// kept as financial records.
func (s *Service) ForgetUser(ctx context.Context, tenantID string, e events.IdentityUserDeletedV1) error {
	if err := validateShopTenant(tenantID); err != nil {
		return err
	}
	return dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		n, err := s.repo.PseudonymizeActor(ctx, tx, tenantID, e.UserID, e.Pseudonym)
		if err != nil {
			return err
		}
		return s.bus.Publish(ctx, tx, events.GDPRCompleted(tenantID, ServiceName, e, events.GDPRPseudonymized, n, s.now().UTC()))
	})
}
