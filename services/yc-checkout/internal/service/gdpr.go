package service

import (
	"context"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
)

// ForgetUser pseudonymises a deleted customer's checkout sessions
// (ADR-005) and reports yc-checkout's part of the cascade.
func (s *Service) ForgetUser(ctx context.Context, tenantID string, e events.IdentityUserDeletedV1) error {
	if tenantID == eventbus.PlatformTenantID || eventbus.ValidateTenantID(tenantID) != nil {
		return ycerrors.Validation().Field("tenant_id", "invalid", "Checkout sessions belong to a shop").Build()
	}
	return dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		n, err := s.repo.Pseudonymise(ctx, tx, tenantID, e.UserID, e.Pseudonym)
		if err != nil {
			return err
		}
		return s.bus.Publish(ctx, tx, events.GDPRCompleted(tenantID, ServiceName, e, events.GDPRPseudonymized, n, s.now()))
	})
}
