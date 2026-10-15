package service

import (
	"context"

	"github.com/jackc/pgx/v5"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	"github.com/slaghuis/yc-platform/services/yc-review/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-review/internal/repository"
)

// AdminReviews returns the shop's reviews, newest first, filtered by
// status and product when set.
func (s *Service) AdminReviews(ctx context.Context, status domain.Status, productID string, after *repository.Cursor, limit int) (Page[domain.Review], error) {
	var page Page[domain.Review]
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		page, err = s.listReviews(ctx, tx, tenantID, repository.Filter{ProductID: productID, Status: status}, after, limit)
		return err
	})
	return page, err
}

// AdminReview returns one of the shop's reviews.
func (s *Service) AdminReview(ctx context.Context, id string) (domain.Review, error) {
	var r domain.Review
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		r, err = s.review(ctx, tx, tenantID, id, false)
		return err
	})
	return r, err
}

// Approve shows a review on its product. A review whose purchase yc-orders
// has not confirmed yet cannot be approved.
func (s *Service) Approve(ctx context.Context, id string) (domain.Review, error) {
	var r domain.Review
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		if r, err = s.review(ctx, tx, tenantID, id, true); err != nil {
			return err
		}
		if r.Status == domain.StatusApproved {
			return nil
		}
		if r.AwaitingPurchase() {
			return ycerrors.Conflict("The purchase is not confirmed yet")
		}
		r.Approve(s.now().UTC())
		return s.approved(ctx, tx, tenantID, r)
	})
	return r, err
}

// Reject hides a pending or approved review, with a reason for the author.
func (s *Service) Reject(ctx context.Context, id, reason string) (domain.Review, error) {
	reason, err := domain.ValidReason(reason)
	if err != nil {
		return domain.Review{}, err
	}
	var r domain.Review
	err = s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		if r, err = s.review(ctx, tx, tenantID, id, true); err != nil {
			return err
		}
		if r.Status == domain.StatusRejected {
			return nil
		}
		r.Reject(reason, s.now().UTC())
		return s.rejected(ctx, tx, tenantID, r)
	})
	return r, err
}

// Respond sets the shop's response to a review, replacing an earlier one.
func (s *Service) Respond(ctx context.Context, id, text string) (domain.Review, error) {
	text, err := domain.ValidResponse(text)
	if err != nil {
		return domain.Review{}, err
	}
	var r domain.Review
	err = s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		if r, err = s.review(ctx, tx, tenantID, id, true); err != nil {
			return err
		}
		now := s.now().UTC()
		r.ShopResponse, r.ShopResponseAt, r.UpdatedAt = text, &now, now
		if err := s.repo.UpdateReview(ctx, tx, tenantID, r); err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypeReviewRespondedV1, events.ReviewRespondedV1{
			ReviewID: r.ID, ProductID: r.ProductID, UserID: r.UserID, RespondedAt: now,
		})
	})
	return r, err
}

// Settings returns the shop's review settings, the defaults until it sets
// its own.
func (s *Service) Settings(ctx context.Context) (domain.Settings, error) {
	var set domain.Settings
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		set, err = s.settings(ctx, tx, tenantID)
		return err
	})
	return set, err
}

// PutSettings replaces the shop's review settings. They apply to reviews
// submitted or edited from now on.
func (s *Service) PutSettings(ctx context.Context, set domain.Settings) (domain.Settings, error) {
	if err := set.Validate(); err != nil {
		return set, err
	}
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		set.UpdatedAt = s.now().UTC()
		return s.repo.PutSettings(ctx, tx, tenantID, set)
	})
	return set, err
}

func (s *Service) review(ctx context.Context, tx pgx.Tx, tenantID, id string, lock bool) (domain.Review, error) {
	r, ok, err := s.repo.Review(ctx, tx, tenantID, id, lock)
	if err == nil && !ok {
		err = ycerrors.NotFound("review", id)
	}
	return r, err
}

// approved stores an approved review and publishes review.approved.v1.
func (s *Service) approved(ctx context.Context, tx pgx.Tx, tenantID string, r domain.Review) error {
	if err := s.repo.UpdateReview(ctx, tx, tenantID, r); err != nil {
		return err
	}
	st, err := s.repo.RefreshStats(ctx, tx, tenantID, r.ProductID)
	if err != nil {
		return err
	}
	return s.publish(ctx, tx, tenantID, events.TypeReviewApprovedV1, events.ReviewApprovedV1{
		ReviewID: r.ID, ProductID: r.ProductID, UserID: r.UserID, Rating: int64(r.Rating),
		ApprovedAt: r.UpdatedAt, ProductStats: productStats(st),
	})
}

// rejected stores a rejected review and publishes review.rejected.v1.
func (s *Service) rejected(ctx context.Context, tx pgx.Tx, tenantID string, r domain.Review) error {
	if err := s.repo.UpdateReview(ctx, tx, tenantID, r); err != nil {
		return err
	}
	st, err := s.repo.RefreshStats(ctx, tx, tenantID, r.ProductID)
	if err != nil {
		return err
	}
	return s.publish(ctx, tx, tenantID, events.TypeReviewRejectedV1, events.ReviewRejectedV1{
		ReviewID: r.ID, ProductID: r.ProductID, UserID: r.UserID, Reason: r.RejectionReason,
		RejectedAt: r.UpdatedAt, ProductStats: productStats(st),
	})
}
