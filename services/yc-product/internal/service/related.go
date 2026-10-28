package service

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	"github.com/slaghuis/yc-platform/services/yc-product/internal/domain"
)

// PutRelated replaces a product's editorial related list with ids, in
// order, and returns the stored list. Targets may be inactive (an admin can
// pre-stage them) but must be live products of the shop.
func (s *Service) PutRelated(ctx context.Context, productID string, ids []string) ([]string, error) {
	ids, err := domain.ValidateRelatedIDs(productID, ids)
	if err != nil {
		return nil, err
	}
	err = s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		// Lock the product and its targets together: a target soft-deleted
		// meanwhile would otherwise keep a link it just had removed.
		live, err := s.cat.LockProducts(ctx, tx, append([]string{productID}, ids...))
		if err != nil {
			return err
		}
		if !live[productID] {
			return ycerrors.NotFound("Product", productID)
		}
		v := ycerrors.Validation()
		for i, id := range ids {
			if !live[id] {
				v.Field(fmt.Sprintf("product_ids[%d]", i), "unknown", "Unknown product")
			}
		}
		if v.HasErrors() {
			return v.Build()
		}
		if err := s.cat.SetRelated(ctx, tx, tenantID, productID, ids); err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypeProductRelatedUpdatedV1, events.ProductRelatedUpdatedV1{
			ProductID: productID, RelatedProductIds: ids, UpdatedAt: s.now().UTC(),
		})
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// AlsoBought returns up to limit active products customers ordered together
// with productID, most often first. The seed is never included. Inactive or
// deleted seeds are not found, except to admins. limit is 1 to
// domain.MaxAlsoBought.
func (s *Service) AlsoBought(ctx context.Context, productID string, limit int) ([]domain.ProductSummary, error) {
	if err := domain.ValidateAlsoBoughtLimit(limit); err != nil {
		return nil, err
	}
	admin := isAdmin(ctx)
	var out []domain.ProductSummary
	err := s.read(ctx, func(tx pgx.Tx) error {
		seed, err := s.cat.GetProduct(ctx, tx, productID, false)
		if err != nil {
			return err
		}
		if seed.Status != domain.StatusActive && !admin {
			return ycerrors.NotFound("Product", productID)
		}
		out, err = s.cat.AlsoBought(ctx, tx, productID, limit)
		return err
	})
	return out, err
}
