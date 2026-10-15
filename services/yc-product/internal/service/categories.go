package service

import (
	"context"

	"github.com/jackc/pgx/v5"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	"github.com/slaghuis/yc-platform/services/yc-product/internal/domain"
)

// CreateCategory creates a category in the caller's shop.
func (s *Service) CreateCategory(ctx context.Context, in domain.CategoryInput) (domain.Category, error) {
	if err := in.Validate(); err != nil {
		return domain.Category{}, err
	}
	var c domain.Category
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		if c, err = s.cat.CreateCategory(ctx, tx, tenantID, in); err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypeProductCategoryCreatedV1, events.ProductCategoryCreatedV1{
			CategoryID: c.ID, ParentID: optional(c.ParentID), Name: c.Name, Slug: c.Slug,
			SortOrder: int64(c.SortOrder), CreatedAt: s.now().UTC(),
		})
	})
	return c, err
}

// UpdateCategory replaces a category's fields. Moving a category under
// itself or a descendant is refused.
func (s *Service) UpdateCategory(ctx context.Context, id string, in domain.CategoryInput) (domain.Category, error) {
	if err := in.Validate(); err != nil {
		return domain.Category{}, err
	}
	var c domain.Category
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		if _, err := s.cat.GetCategory(ctx, tx, id); err != nil {
			return err
		}
		if in.ParentID != "" {
			cycle, err := s.cat.IsSelfOrDescendant(ctx, tx, id, in.ParentID)
			if err != nil {
				return err
			}
			if cycle {
				return ycerrors.Conflict("A category cannot move under itself or a subcategory")
			}
		}
		var err error
		if c, err = s.cat.UpdateCategory(ctx, tx, id, in); err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypeProductCategoryUpdatedV1, events.ProductCategoryUpdatedV1{
			CategoryID: c.ID, ParentID: optional(c.ParentID), Name: c.Name, Slug: c.Slug,
			SortOrder: int64(c.SortOrder), UpdatedAt: s.now().UTC(),
		})
	})
	return c, err
}

// DeleteCategory deletes a category without subcategories; its products
// leave it.
func (s *Service) DeleteCategory(ctx context.Context, id string) error {
	return s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		products, err := s.cat.DeleteCategory(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := s.cat.RefreshProjection(ctx, tx, products...); err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypeProductCategoryDeletedV1, events.ProductCategoryDeletedV1{
			CategoryID: id, DeletedAt: s.now().UTC(),
		})
	})
}

// CategoryTree returns the caller's shop's category tree.
func (s *Service) CategoryTree(ctx context.Context) ([]*domain.CategoryNode, error) {
	var cats []domain.Category
	err := s.read(ctx, func(tx pgx.Tx) error {
		var err error
		cats, err = s.cat.Categories(ctx, tx)
		return err
	})
	if err != nil {
		return nil, err
	}
	return domain.BuildTree(cats), nil
}
