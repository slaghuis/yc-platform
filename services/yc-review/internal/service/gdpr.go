package service

import (
	"context"

	"github.com/jackc/pgx/v5"

	events "github.com/slaghuis/yc-platform/libs/yc-events"
)

// ForgetUser handles a deleted customer (ADR-005): their reviews keep
// their text under the pseudonym, their likes are removed with the
// products' stats refreshed, and pending review prompts are dropped.
func (s *Service) ForgetUser(ctx context.Context, tenantID string, e events.IdentityUserDeletedV1) error {
	ctx = serviceContext(ctx, tenantID)
	return s.inTenant(ctx, tenantID, func(tx pgx.Tx, tenantID string) error {
		f, err := s.repo.ForgetUser(ctx, tx, tenantID, e.UserID, e.Pseudonym)
		if err != nil {
			return err
		}
		now := s.now().UTC()
		for _, productID := range f.LikedProducts {
			st, err := s.repo.RefreshStats(ctx, tx, tenantID, productID)
			if err != nil {
				return err
			}
			err = s.publish(ctx, tx, tenantID, events.TypeReviewLikeRemovedV1, events.ReviewLikeRemovedV1{
				ProductID: productID, UserID: e.Pseudonym, UnlikedAt: now, ProductStats: productStats(st),
			})
			if err != nil {
				return err
			}
		}
		n := f.Reviews + f.Prompts + int64(len(f.LikedProducts))
		return s.bus.Publish(ctx, tx, events.GDPRCompleted(tenantID, ServiceName, e, events.GDPRPseudonymized, n, now))
	})
}
