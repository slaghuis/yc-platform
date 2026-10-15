package service

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	"github.com/slaghuis/yc-platform/services/yc-review/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-review/internal/repository"
)

// OrderDelivered schedules a review prompt for a delivered order. Whether
// the buyer is a customer, and what they bought, is read from yc-orders
// when the prompt is due.
func (s *Service) OrderDelivered(ctx context.Context, tenantID string, p events.OrderDeliveredV1) error {
	if p.UserID == "" {
		return nil
	}
	return s.inTenant(ctx, tenantID, func(tx pgx.Tx, tenantID string) error {
		return s.repo.InsertPrompt(ctx, tx, tenantID, p.OrderID, p.UserID, p.DeliveredAt.Add(s.cfg.PromptDelay))
	})
}

// RunJobs checks deferred purchases and sends due review prompts every
// interval until ctx is done.
func (s *Service) RunJobs(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := s.CheckPurchases(ctx); err != nil && ctx.Err() == nil {
			telemetry.LoggerFromContext(ctx).Error("purchase checks", "err", err)
		}
		if _, err := s.SendPrompts(ctx); err != nil && ctx.Err() == nil {
			telemetry.LoggerFromContext(ctx).Error("review prompts", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// CheckPurchases asks yc-orders again about reviews whose purchase it could
// not confirm at submission, and returns how many it tried. A confirmed
// purchase verifies the review (and approves it in auto-approving shops),
// an unknown one rejects it, and after the window it is rejected as
// unverifiable.
func (s *Service) CheckPurchases(ctx context.Context) (int, error) {
	due, err := s.due(ctx, s.repo.DueChecks)
	if err != nil {
		return 0, err
	}
	var errs []error
	for _, d := range due {
		if ctx.Err() != nil {
			break
		}
		errs = append(errs, s.checkPurchase(serviceContext(ctx, d.TenantID), d))
	}
	return len(due), errors.Join(errs...)
}

func (s *Service) checkPurchase(ctx context.Context, d repository.Due) error {
	return s.inTenant(ctx, d.TenantID, func(tx pgx.Tx, tenantID string) error {
		now := s.now().UTC()
		r, ok, err := s.repo.LockDueCheck(ctx, tx, tenantID, d.ID, now)
		if err != nil || !ok {
			return err
		}
		p, err := s.orders.LatestPurchase(ctx, r.UserID, r.ProductID)
		switch {
		case err == nil && p.Purchased:
			set, err := s.settings(ctx, tx, tenantID)
			if err != nil {
				return err
			}
			r.VerifiedPurchase, r.PurchaseOrderID = true, p.OrderID
			r.PurchaseCheckUntil, r.NextCheckAt, r.UpdatedAt = nil, nil, now
			if set.ModerationMode == domain.ModerationAuto {
				r.Approve(now)
				return s.approved(ctx, tx, tenantID, r)
			}
			return s.repo.UpdateReview(ctx, tx, tenantID, r)
		case err == nil:
			r.Reject(domain.ReasonNotPurchased, now)
			return s.rejected(ctx, tx, tenantID, r)
		case !now.Before(*r.PurchaseCheckUntil):
			telemetry.LoggerFromContext(ctx).Warn("purchase check expired", "review_id", r.ID, "err", err)
			r.Reject(domain.ReasonCheckExpired, now)
			return s.rejected(ctx, tx, tenantID, r)
		default:
			next := now.Add(s.cfg.CheckRetry)
			r.NextCheckAt = &next
			return s.repo.UpdateReview(ctx, tx, tenantID, r)
		}
	})
}

// SendPrompts publishes review.prompt_requested.v1 for due prompts: to
// customers (not guests), for the order's products they have not reviewed
// yet. It returns how many prompts it tried.
func (s *Service) SendPrompts(ctx context.Context) (int, error) {
	due, err := s.due(ctx, s.repo.DuePrompts)
	if err != nil {
		return 0, err
	}
	var errs []error
	for _, d := range due {
		if ctx.Err() != nil {
			break
		}
		errs = append(errs, s.sendPrompt(serviceContext(ctx, d.TenantID), d))
	}
	return len(due), errors.Join(errs...)
}

func (s *Service) sendPrompt(ctx context.Context, d repository.Due) error {
	return s.inTenant(ctx, d.TenantID, func(tx pgx.Tx, tenantID string) error {
		now := s.now().UTC()
		p, ok, err := s.repo.LockDuePrompt(ctx, tx, tenantID, d.ID, now)
		if err != nil || !ok {
			return err
		}
		o, err := s.orders.Order(ctx, p.OrderID)
		if httpkit.UpstreamStatus(err) == http.StatusNotFound {
			return s.repo.FinishPrompt(ctx, tx, tenantID, p.OrderID, now)
		}
		if err != nil {
			telemetry.LoggerFromContext(ctx).Warn("review prompt delayed", "order_id", p.OrderID, "err", err)
			return s.repo.DelayPrompt(ctx, tx, tenantID, p.OrderID, now.Add(s.cfg.CheckRetry))
		}
		if o.UserType != "customer" || o.UserID != p.UserID {
			return s.repo.FinishPrompt(ctx, tx, tenantID, p.OrderID, now)
		}
		var ids []string
		names := map[string]string{}
		for _, it := range o.Items {
			if _, seen := names[it.ProductID]; it.ProductID == "" || seen {
				continue
			}
			names[it.ProductID] = it.NameSnapshot
			ids = append(ids, it.ProductID)
		}
		reviewed, err := s.repo.Reviewed(ctx, tx, tenantID, p.UserID, ids)
		if err != nil {
			return err
		}
		var products []events.ReviewPromptRequestedV1ProductsItem
		for _, id := range ids {
			if !reviewed[id] {
				products = append(products, events.ReviewPromptRequestedV1ProductsItem{ProductID: id, Name: names[id]})
			}
		}
		if err := s.repo.FinishPrompt(ctx, tx, tenantID, p.OrderID, now); err != nil || len(products) == 0 {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypeReviewPromptRequestedV1, events.ReviewPromptRequestedV1{
			OrderID: o.OrderID, OrderNumber: o.OrderNumber, UserID: p.UserID, Products: products, RequestedAt: now,
		})
	})
}

// due lists due rows across shops.
func (s *Service) due(ctx context.Context, list func(context.Context, pgx.Tx, time.Time, int) ([]repository.Due, error)) ([]repository.Due, error) {
	var due []repository.Due
	err := dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		due, err = list(ctx, tx, s.now().UTC(), s.cfg.BatchSize)
		return err
	})
	return due, err
}
