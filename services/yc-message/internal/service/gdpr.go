package service

import (
	"context"

	"github.com/jackc/pgx/v5"

	events "github.com/slaghuis/yc-platform/libs/yc-events"
)

// ForgetUser pseudonymises a deleted user's messages, cancels those still
// queued, deletes their verification codes and reports yc-message's part
// of the cascade (ADR-005).
func (s *Service) ForgetUser(ctx context.Context, tenantID string, e events.IdentityUserDeletedV1) error {
	return s.inTenant(ctx, tenantID, func(tx pgx.Tx, tenantID string) error {
		now := s.now().UTC()
		n, err := s.repo.ForgetRecipient(ctx, tx, tenantID, e.UserID, e.Pseudonym, "cancelled: recipient deleted", now)
		if err != nil {
			return err
		}
		return s.bus.Publish(ctx, tx, events.GDPRCompleted(tenantID, ServiceName, e, events.GDPRPseudonymized, n, now))
	})
}
