package service

import (
	"context"

	"github.com/jackc/pgx/v5"

	events "github.com/slaghuis/yc-platform/libs/yc-events"
	"github.com/slaghuis/yc-platform/services/yc-review/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-review/internal/repository"
)

// Like records that the calling customer likes a product. Liking it again
// changes nothing.
func (s *Service) Like(ctx context.Context, productID string) (domain.Stats, error) {
	var st domain.Stats
	err := s.asCustomer(ctx, func(tx pgx.Tx, tenantID, userID string) error {
		now := s.now().UTC()
		added, err := s.repo.AddLike(ctx, tx, tenantID, productID, userID, now)
		if err != nil {
			return err
		}
		if !added {
			st, err = s.repo.Stats(ctx, tx, tenantID, productID)
			return err
		}
		if st, err = s.repo.RefreshStats(ctx, tx, tenantID, productID); err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypeReviewLikeAddedV1, events.ReviewLikeAddedV1{
			ProductID: productID, UserID: userID, LikedAt: now, ProductStats: productStats(st),
		})
	})
	return st, err
}

// Unlike removes the calling customer's like. Without one it changes
// nothing.
func (s *Service) Unlike(ctx context.Context, productID string) (domain.Stats, error) {
	var st domain.Stats
	err := s.asCustomer(ctx, func(tx pgx.Tx, tenantID, userID string) error {
		removed, err := s.repo.RemoveLike(ctx, tx, tenantID, productID, userID)
		if err != nil {
			return err
		}
		if !removed {
			st, err = s.repo.Stats(ctx, tx, tenantID, productID)
			return err
		}
		if st, err = s.repo.RefreshStats(ctx, tx, tenantID, productID); err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypeReviewLikeRemovedV1, events.ReviewLikeRemovedV1{
			ProductID: productID, UserID: userID, UnlikedAt: s.now().UTC(), ProductStats: productStats(st),
		})
	})
	return st, err
}

// MyLikes returns the products the calling customer likes, newest first.
func (s *Service) MyLikes(ctx context.Context, after *repository.Cursor, limit int) (Page[repository.Like], error) {
	var page Page[repository.Like]
	err := s.asCustomer(ctx, func(tx pgx.Tx, tenantID, userID string) error {
		rows, err := s.repo.Likes(ctx, tx, tenantID, userID, after, limit+1)
		if err != nil {
			return err
		}
		page.Items = rows
		if len(rows) > limit {
			page.Items = rows[:limit]
			last := page.Items[limit-1]
			page.Next = &repository.Cursor{CreatedAt: last.LikedAt, ID: last.ProductID}
		}
		return nil
	})
	return page, err
}
