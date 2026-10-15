package service

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	"github.com/slaghuis/yc-platform/services/yc-review/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-review/internal/repository"
)

// Submit records the calling customer's review of a product. When the shop
// requires a purchase, yc-orders must confirm one: a customer who did not
// buy the product gets a conflict, and while yc-orders cannot answer the
// review waits pending and is checked again (CheckPurchases).
func (s *Service) Submit(ctx context.Context, productID string, c domain.Content) (domain.Review, error) {
	c.Normalise()
	if err := c.Validate(); err != nil {
		return domain.Review{}, err
	}
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorCustomer || a.ActorID == "" {
		return domain.Review{}, ycerrors.Forbidden("Sign in to review products").WithInternal("actor_type %s", a.ActorType)
	}
	var set domain.Settings
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		if set, err = s.settings(ctx, tx, tenantID); err != nil {
			return err
		}
		mine, err := s.repo.ListReviews(ctx, tx, tenantID, repository.Filter{ProductID: productID, UserID: a.ActorID}, nil, 1)
		if err == nil && len(mine) > 0 {
			return ycerrors.Conflict("You have already reviewed this product; edit your review instead")
		}
		return err
	})
	if err != nil {
		return domain.Review{}, err
	}

	now := s.now().UTC()
	r := domain.Review{ID: uuid.NewString(), ProductID: productID, UserID: a.ActorID, Content: c, CreatedAt: now}
	p, err := s.orders.LatestPurchase(ctx, a.ActorID, productID)
	switch {
	case err == nil && p.Purchased:
		r.VerifiedPurchase, r.PurchaseOrderID = true, p.OrderID
	case err == nil && set.RequirePurchase:
		return domain.Review{}, ycerrors.Conflict("Only customers who bought this product can review it")
	case err != nil && set.RequirePurchase:
		telemetry.LoggerFromContext(ctx).Warn("purchase check deferred", "product_id", productID, "err", err)
		until, next := now.Add(s.cfg.CheckWindow), now.Add(s.cfg.CheckRetry)
		r.PurchaseCheckUntil, r.NextCheckAt = &until, &next
	case err != nil:
		telemetry.LoggerFromContext(ctx).Warn("purchase check failed; review is unverified", "product_id", productID, "err", err)
	}
	r.Moderate(set.ModerationMode, now)

	err = s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		err := s.repo.InsertReview(ctx, tx, tenantID, r)
		if dbkit.IsUniqueViolation(err) {
			return ycerrors.Conflict("You have already reviewed this product; edit your review instead")
		}
		if err != nil {
			return err
		}
		st, err := s.repo.RefreshStats(ctx, tx, tenantID, productID)
		if err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypeReviewSubmittedV1, events.ReviewSubmittedV1{
			ReviewID: r.ID, ProductID: productID, UserID: r.UserID, Rating: int64(r.Rating), Status: string(r.Status),
			VerifiedPurchase: r.VerifiedPurchase, SubmittedAt: now, ProductStats: productStats(st),
		})
	})
	return r, err
}

// Edit replaces the author's review content. The review goes back to
// moderation unless the shop auto-approves.
func (s *Service) Edit(ctx context.Context, id string, c domain.Content) (domain.Review, error) {
	c.Normalise()
	if err := c.Validate(); err != nil {
		return domain.Review{}, err
	}
	var r domain.Review
	err := s.asCustomer(ctx, func(tx pgx.Tx, tenantID, userID string) error {
		var err error
		if r, err = s.ownReview(ctx, tx, tenantID, userID, id); err != nil {
			return err
		}
		set, err := s.settings(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		if !set.AllowEdit {
			return ycerrors.Forbidden("This shop does not allow editing reviews")
		}
		if r.Status == domain.StatusRejected && (r.RejectionReason == domain.ReasonNotPurchased || r.RejectionReason == domain.ReasonCheckExpired) {
			return ycerrors.Conflict("The review was rejected because the purchase could not be verified")
		}
		now := s.now().UTC()
		r.Content = c
		r.Moderate(set.ModerationMode, now)
		if err := s.repo.UpdateReview(ctx, tx, tenantID, r); err != nil {
			return err
		}
		st, err := s.repo.RefreshStats(ctx, tx, tenantID, r.ProductID)
		if err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypeReviewEditedV1, events.ReviewEditedV1{
			ReviewID: r.ID, ProductID: r.ProductID, UserID: r.UserID, Rating: int64(r.Rating), Status: string(r.Status),
			EditedAt: now, ProductStats: productStats(st),
		})
	})
	return r, err
}

// Delete removes the author's review.
func (s *Service) Delete(ctx context.Context, id string) error {
	return s.asCustomer(ctx, func(tx pgx.Tx, tenantID, userID string) error {
		r, err := s.ownReview(ctx, tx, tenantID, userID, id)
		if err != nil {
			return err
		}
		if err := s.repo.DeleteReview(ctx, tx, tenantID, id); err != nil {
			return err
		}
		st, err := s.repo.RefreshStats(ctx, tx, tenantID, r.ProductID)
		if err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypeReviewDeletedV1, events.ReviewDeletedV1{
			ReviewID: r.ID, ProductID: r.ProductID, UserID: r.UserID, DeletedAt: s.now().UTC(), ProductStats: productStats(st),
		})
	})
}

// ownReview returns the user's review, locked. Someone else's review is
// not found.
func (s *Service) ownReview(ctx context.Context, tx pgx.Tx, tenantID, userID, id string) (domain.Review, error) {
	r, ok, err := s.repo.Review(ctx, tx, tenantID, id, true)
	if err != nil {
		return r, err
	}
	if !ok || r.UserID != userID {
		return r, ycerrors.NotFound("review", id)
	}
	return r, nil
}

// Page is a page of rows and the cursor of the next, if any.
type Page[T any] struct {
	Items []T
	Next  *repository.Cursor
}

// ProductReviews returns a product's approved reviews, newest first.
func (s *Service) ProductReviews(ctx context.Context, productID string, after *repository.Cursor, limit int) (Page[domain.Review], error) {
	var page Page[domain.Review]
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		page, err = s.listReviews(ctx, tx, tenantID, repository.Filter{ProductID: productID, Status: domain.StatusApproved}, after, limit)
		return err
	})
	return page, err
}

// MyReviews returns the calling customer's reviews in any status, of one
// product when productID is set.
func (s *Service) MyReviews(ctx context.Context, productID string, after *repository.Cursor, limit int) (Page[domain.Review], error) {
	var page Page[domain.Review]
	err := s.asCustomer(ctx, func(tx pgx.Tx, tenantID, userID string) error {
		var err error
		page, err = s.listReviews(ctx, tx, tenantID, repository.Filter{ProductID: productID, UserID: userID}, after, limit)
		return err
	})
	return page, err
}

func (s *Service) listReviews(ctx context.Context, tx pgx.Tx, tenantID string, f repository.Filter, after *repository.Cursor, limit int) (Page[domain.Review], error) {
	rows, err := s.repo.ListReviews(ctx, tx, tenantID, f, after, limit+1)
	if err != nil {
		return Page[domain.Review]{}, err
	}
	page := Page[domain.Review]{Items: rows}
	if len(rows) > limit {
		page.Items = rows[:limit]
		last := page.Items[limit-1]
		page.Next = &repository.Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, nil
}

// ProductSummary is a product's stats and rating distribution.
type ProductSummary struct {
	domain.Stats
	// Distribution counts approved reviews by rating; index 0 is one star.
	Distribution [domain.MaxRating]int64
}

// ProductStats returns a product's review and like summary.
func (s *Service) ProductStats(ctx context.Context, productID string) (ProductSummary, error) {
	var out ProductSummary
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		if out.Stats, err = s.repo.Stats(ctx, tx, tenantID, productID); err != nil {
			return err
		}
		out.Distribution, err = s.repo.Distribution(ctx, tx, tenantID, productID)
		return err
	})
	return out, err
}
