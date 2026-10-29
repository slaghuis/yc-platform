package service

import (
	"context"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
)

// ForgetUser erases a deleted user's part of the support threads (ADR-005)
// and reports yc-support's part of the cascade.
//
// The user's requests lose their contact name, email, phone and page URL,
// their message bodies are replaced, and the user ID is detached, so the
// shop keeps a count of what was asked but no person. A deleted shop admin
// keeps their messages with the author cleared. Redelivery finds nothing
// left to change.
func (s *Service) ForgetUser(ctx context.Context, tenantID string, e events.IdentityUserDeletedV1) error {
	if err := validateShopTenant(tenantID); err != nil {
		return err
	}
	return dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		now := s.now().UTC()
		ids, err := s.requests.Redact(ctx, tx, tenantID, e.UserID, now)
		if err != nil {
			return err
		}
		bodies, err := s.messages.Redact(ctx, tx, tenantID, ids)
		if err != nil {
			return err
		}
		authored, err := s.messages.DetachAuthor(ctx, tx, tenantID, e.UserID)
		if err != nil {
			return err
		}
		records := int64(len(ids)) + bodies + authored
		return s.bus.Publish(ctx, tx, events.GDPRCompleted(tenantID, ServiceName, e, events.GDPRAnonymized, records, now))
	})
}
