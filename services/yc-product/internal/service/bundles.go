package service

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	"github.com/slaghuis/yc-platform/services/yc-product/internal/domain"
)

// CreateBundle creates a physical kit product (kind=bundle) with one
// untracked SKU and ≥2 component SKUs.
func (s *Service) CreateBundle(ctx context.Context, in domain.BundleInput) (domain.Product, error) {
	if err := in.Validate(); err != nil {
		return domain.Product{}, err
	}
	var p domain.Product
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		if err := s.lockEligibleComponents(ctx, tx, in.Components); err != nil {
			return err
		}
		var err error
		p, err = s.cat.CreateProduct(ctx, tx, tenantID, domain.ProductInput{
			Name: in.Name, Description: in.Description, IsDigital: false,
			Kind: domain.KindBundle, Status: domain.StatusActive, CategoryIDs: in.CategoryIDs,
			Attributes: map[string]string{},
		})
		if err != nil {
			return err
		}
		sku, err := s.cat.CreateSKU(ctx, tx, p, domain.SKUInput{
			Code: in.SKUCode, VariantValues: map[string]string{}, InventoryTracked: false,
			TaxClass: in.TaxClass, Status: domain.StatusActive,
		})
		if err != nil {
			return err
		}
		if err := s.cat.SetBundleComponents(ctx, tx, tenantID, sku.ID, in.Components); err != nil {
			return err
		}
		if err := s.cat.RefreshProjection(ctx, tx, p.ID); err != nil {
			return err
		}
		if err := s.cat.RecomputeBundleInStock(ctx, tx, sku.ID); err != nil {
			return err
		}
		if err := s.publish(ctx, tx, tenantID, events.TypeProductCreatedV1, events.ProductCreatedV1{
			ProductID: p.ID, SupplierID: optional(p.SupplierID), Name: p.Name, Status: string(p.Status),
			IsDigital: false, CategoryIds: p.CategoryIDs, CreatedAt: p.CreatedAt.UTC(),
		}); err != nil {
			return err
		}
		if err := s.publish(ctx, tx, tenantID, events.TypeProductSKUCreatedV1, events.ProductSKUCreatedV1{
			SKUID: sku.ID, ProductID: p.ID, SKUCode: sku.Code, VariantValues: map[string]any{},
			IsInventoryTracked: false, TaxClass: sku.TaxClass, Status: string(sku.Status),
			CreatedAt: sku.CreatedAt.UTC(),
		}); err != nil {
			return err
		}
		return s.cat.LoadChildren(ctx, tx, &p, true)
	})
	return p, err
}

// UpdateBundle replaces a kit's components and optionally name / description /
// categories. The product must already be kind=bundle.
func (s *Service) UpdateBundle(ctx context.Context, productID string, in domain.BundleUpdateInput) (domain.Product, error) {
	if err := in.Validate(); err != nil {
		return domain.Product{}, err
	}
	var p domain.Product
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		cur, err := s.cat.GetProduct(ctx, tx, productID, true)
		if err != nil {
			return err
		}
		if cur.Kind != domain.KindBundle {
			return ycerrors.Validation().Field("product_id", "invalid", "Not a bundle product").Build()
		}
		if err := s.lockEligibleComponents(ctx, tx, in.Components); err != nil {
			return err
		}
		p, err = s.cat.UpdateBundleProduct(ctx, tx, productID, in.Name, in.Description, in.CategoryIDs)
		if err != nil {
			return err
		}
		skus, err := s.cat.SKUsOf(ctx, tx, []string{productID}, true)
		if err != nil {
			return err
		}
		list := skus[productID]
		if len(list) == 0 {
			return ycerrors.Conflict("Bundle has no SKU")
		}
		bundleSKU := list[0]
		if err := s.cat.SetBundleComponents(ctx, tx, tenantID, bundleSKU.ID, in.Components); err != nil {
			return err
		}
		if err := s.cat.RefreshProjection(ctx, tx, p.ID); err != nil {
			return err
		}
		if err := s.cat.RecomputeBundleInStock(ctx, tx, bundleSKU.ID); err != nil {
			return err
		}
		if err := s.publish(ctx, tx, tenantID, events.TypeProductUpdatedV1, events.ProductUpdatedV1{
			ProductID: p.ID, SupplierID: optional(p.SupplierID), Name: p.Name, Status: string(p.Status),
			IsDigital: false, CategoryIds: p.CategoryIDs, UpdatedAt: p.UpdatedAt.UTC(),
		}); err != nil {
			return err
		}
		return s.cat.LoadChildren(ctx, tx, &p, true)
	})
	return p, err
}

// GetBundle returns a kit product with its composition. Non-bundle products
// are not found on this path.
func (s *Service) GetBundle(ctx context.Context, productID string) (domain.Product, error) {
	var p domain.Product
	err := s.inShop(ctx, func(tx pgx.Tx, _ string) error {
		var err error
		if p, err = s.cat.GetProduct(ctx, tx, productID, false); err != nil {
			return err
		}
		if p.Kind != domain.KindBundle {
			return ycerrors.NotFound("Bundle", productID)
		}
		return s.cat.LoadChildren(ctx, tx, &p, true)
	})
	return p, err
}

// lockEligibleComponents locks comps and verifies each is an active,
// physical, non-bundle SKU of the shop.
func (s *Service) lockEligibleComponents(ctx context.Context, tx pgx.Tx, comps []domain.BundleComponentInput) error {
	ids := make([]string, len(comps))
	for i, c := range comps {
		ids[i] = c.SKUID
	}
	found, err := s.cat.LockComponentSKUs(ctx, tx, ids)
	if err != nil {
		return err
	}
	v := ycerrors.Validation()
	for i, c := range comps {
		field := fmt.Sprintf("components[%d]", i)
		got, ok := found[c.SKUID]
		switch {
		case !ok:
			v.Field(field+".sku_id", "unknown", "Unknown SKU")
		case got.Kind == domain.KindBundle:
			v.Field(field+".sku_id", "nested_bundle", "Components must be non-bundle SKUs")
		case got.IsDigital:
			v.Field(field+".sku_id", "digital", "Bundle components must be physical")
		case got.Status != domain.StatusActive || got.ProductStatus != domain.StatusActive:
			v.Field(field+".sku_id", "inactive", "Component SKU must be active")
		}
	}
	if v.HasErrors() {
		return v.Build()
	}
	return nil
}

// softDeleteProduct deletes one product, refreshes its projection and
// publishes product.deleted.v1. Caller holds the shop transaction.
func (s *Service) softDeleteProduct(ctx context.Context, tx pgx.Tx, tenantID, id string) error {
	skuIDs, err := s.cat.SoftDeleteProduct(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := s.cat.RefreshProjection(ctx, tx, id); err != nil {
		return err
	}
	return s.publish(ctx, tx, tenantID, events.TypeProductDeletedV1, events.ProductDeletedV1{
		ProductID: id, SKUIds: skuIDs, DeletedAt: s.now().UTC(),
	})
}

// retireDependentBundles soft-deletes every live kit that includes the
// given component product or SKU.
func (s *Service) retireDependentBundles(ctx context.Context, tx pgx.Tx, tenantID, componentProductID, componentSKUID string) error {
	ids, err := s.cat.BundleProductsUsingComponent(ctx, tx, componentProductID, componentSKUID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.softDeleteProduct(ctx, tx, tenantID, id); err != nil {
			return err
		}
	}
	return nil
}
