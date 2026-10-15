package service

import (
	"context"

	"github.com/jackc/pgx/v5"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	"github.com/slaghuis/yc-platform/services/yc-product/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-product/internal/repository"
)

// CreateProduct creates a product in the caller's shop.
func (s *Service) CreateProduct(ctx context.Context, in domain.ProductInput) (domain.Product, error) {
	if err := in.Validate(); err != nil {
		return domain.Product{}, err
	}
	var p domain.Product
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		if p, err = s.cat.CreateProduct(ctx, tx, tenantID, in); err != nil {
			return err
		}
		p.Variants, p.SKUs, p.Images = []domain.Variant{}, []domain.SKU{}, []domain.Image{}
		return s.publish(ctx, tx, tenantID, events.TypeProductCreatedV1, events.ProductCreatedV1{
			ProductID: p.ID, SupplierID: optional(p.SupplierID), Name: p.Name, Status: string(p.Status),
			IsDigital: p.IsDigital, CategoryIds: p.CategoryIDs, CreatedAt: p.CreatedAt.UTC(),
		})
	})
	return p, err
}

// UpdateProduct replaces a product's fields.
func (s *Service) UpdateProduct(ctx context.Context, id string, in domain.ProductInput) (domain.Product, error) {
	if err := in.Validate(); err != nil {
		return domain.Product{}, err
	}
	var p domain.Product
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		if p, err = s.cat.UpdateProduct(ctx, tx, id, in); err != nil {
			return err
		}
		if err := s.cat.LoadChildren(ctx, tx, &p, true); err != nil {
			return err
		}
		if err := s.cat.RefreshProjection(ctx, tx, p.ID); err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypeProductUpdatedV1, events.ProductUpdatedV1{
			ProductID: p.ID, SupplierID: optional(p.SupplierID), Name: p.Name, Status: string(p.Status),
			IsDigital: p.IsDigital, CategoryIds: p.CategoryIDs, UpdatedAt: p.UpdatedAt.UTC(),
		})
	})
	return p, err
}

// DeleteProduct soft-deletes a product and its SKUs.
func (s *Service) DeleteProduct(ctx context.Context, id string) error {
	return s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
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
	})
}

// AddVariant adds a variant axis to a product that has no SKUs yet.
func (s *Service) AddVariant(ctx context.Context, productID string, in domain.VariantInput) (domain.Variant, error) {
	if err := in.Validate(); err != nil {
		return domain.Variant{}, err
	}
	var va domain.Variant
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		p, err := s.cat.GetProduct(ctx, tx, productID, true)
		if err != nil {
			return err
		}
		n, err := s.cat.CountSKUs(ctx, tx, productID)
		if err != nil {
			return err
		}
		if n > 0 {
			return ycerrors.Conflict("Add variant axes before creating SKUs")
		}
		if va, err = s.cat.AddVariant(ctx, tx, p, in); err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypeProductVariantAddedV1, events.ProductVariantAddedV1{
			VariantID: va.ID, ProductID: p.ID, AxisName: va.AxisName, Values: va.Values, AddedAt: va.CreatedAt.UTC(),
		})
	})
	return va, err
}

// CreateSKU creates a SKU of a product.
func (s *Service) CreateSKU(ctx context.Context, productID string, in domain.SKUInput) (domain.SKU, error) {
	if err := in.Validate(); err != nil {
		return domain.SKU{}, err
	}
	var sku domain.SKU
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		p, err := s.cat.GetProduct(ctx, tx, productID, true)
		if err != nil {
			return err
		}
		if err := s.checkVariants(ctx, tx, p.ID, in.VariantValues, ""); err != nil {
			return err
		}
		if sku, err = s.cat.CreateSKU(ctx, tx, p, in); err != nil {
			return err
		}
		if err := s.cat.RefreshProjection(ctx, tx, p.ID); err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypeProductSKUCreatedV1, events.ProductSKUCreatedV1{
			SKUID: sku.ID, ProductID: p.ID, SKUCode: sku.Code, VariantValues: anyMap(sku.VariantValues),
			IsInventoryTracked: sku.InventoryTracked, TaxClass: sku.TaxClass, Status: string(sku.Status),
			CreatedAt: sku.CreatedAt.UTC(),
		})
	})
	return sku, err
}

// UpdateSKU replaces a SKU's fields.
func (s *Service) UpdateSKU(ctx context.Context, id string, in domain.SKUInput) (domain.SKU, error) {
	if err := in.Validate(); err != nil {
		return domain.SKU{}, err
	}
	var sku domain.SKU
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		cur, err := s.cat.GetSKU(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if err := s.checkVariants(ctx, tx, cur.ProductID, in.VariantValues, id); err != nil {
			return err
		}
		if sku, err = s.cat.UpdateSKU(ctx, tx, id, in); err != nil {
			return err
		}
		if err := s.cat.RefreshProjection(ctx, tx, sku.ProductID); err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypeProductSKUUpdatedV1, events.ProductSKUUpdatedV1{
			SKUID: sku.ID, ProductID: sku.ProductID, SKUCode: sku.Code, VariantValues: anyMap(sku.VariantValues),
			IsInventoryTracked: sku.InventoryTracked, TaxClass: sku.TaxClass, Status: string(sku.Status),
			UpdatedAt: sku.UpdatedAt.UTC(),
		})
	})
	return sku, err
}

// DeleteSKU soft-deletes a SKU.
func (s *Service) DeleteSKU(ctx context.Context, id string) error {
	return s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		cur, err := s.cat.GetSKU(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if err := s.cat.SoftDeleteSKU(ctx, tx, id); err != nil {
			return err
		}
		if err := s.cat.RefreshProjection(ctx, tx, cur.ProductID); err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypeProductSKUDeletedV1, events.ProductSKUDeletedV1{
			SKUID: id, ProductID: cur.ProductID, DeletedAt: s.now().UTC(),
		})
	})
}

// AttachImage attaches a yc-image image to a product.
func (s *Service) AttachImage(ctx context.Context, productID string, img domain.Image) (domain.Image, error) {
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		p, err := s.cat.GetProduct(ctx, tx, productID, true)
		if err != nil {
			return err
		}
		if err := s.cat.AttachImage(ctx, tx, p, img); err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypeProductImageAttachedV1, events.ProductImageAttachedV1{
			ProductID: p.ID, ImageID: img.ImageID, SortOrder: int64(img.SortOrder), IsPrimary: img.IsPrimary,
			AttachedAt: s.now().UTC(),
		})
	})
	return img, err
}

// DetachImage detaches an image from a product.
func (s *Service) DetachImage(ctx context.Context, productID, imageID string) error {
	return s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		if _, err := s.cat.GetProduct(ctx, tx, productID, true); err != nil {
			return err
		}
		if err := s.cat.DetachImage(ctx, tx, productID, imageID); err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypeProductImageDetachedV1, events.ProductImageDetachedV1{
			ProductID: productID, ImageID: imageID, DetachedAt: s.now().UTC(),
		})
	})
}

// GetProduct returns a product with its children. Inactive products and
// SKUs are visible to admins only.
func (s *Service) GetProduct(ctx context.Context, id string) (domain.Product, error) {
	admin := isAdmin(ctx)
	var p domain.Product
	err := s.read(ctx, func(tx pgx.Tx) error {
		var err error
		if p, err = s.cat.GetProduct(ctx, tx, id, false); err != nil {
			return err
		}
		if p.Status != domain.StatusActive && !admin {
			return ycerrors.NotFound("Product", id)
		}
		return s.cat.LoadChildren(ctx, tx, &p, admin)
	})
	return p, err
}

// GetSKU returns a SKU. Inactive SKUs, and SKUs of inactive products, are
// visible to admins only.
func (s *Service) GetSKU(ctx context.Context, id string) (domain.SKU, error) {
	admin := isAdmin(ctx)
	var sku domain.SKU
	err := s.read(ctx, func(tx pgx.Tx) error {
		var err error
		if sku, err = s.cat.GetSKU(ctx, tx, id, false); err != nil {
			return err
		}
		if admin {
			return nil
		}
		p, err := s.cat.GetProduct(ctx, tx, sku.ProductID, false)
		if ycerrors.HasCode(err, ycerrors.CodeNotFound) || (err == nil && (p.Status != domain.StatusActive || sku.Status != domain.StatusActive)) {
			return ycerrors.NotFound("SKU", id)
		}
		return err
	})
	return sku, err
}

// Browse returns a page of active products matching f.
func (s *Service) Browse(ctx context.Context, f domain.BrowseFilter, after *repository.Cursor, limit int) ([]domain.ProductSummary, *repository.Cursor, error) {
	if err := f.Validate(); err != nil {
		return nil, nil, err
	}
	var out []domain.ProductSummary
	err := s.read(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = s.cat.Browse(ctx, tx, f, after, limit+1)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	var next *repository.Cursor
	if len(out) > limit {
		out = out[:limit]
		last := out[limit-1]
		next = &repository.Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return out, next, nil
}

func (s *Service) checkVariants(ctx context.Context, tx pgx.Tx, productID string, values map[string]string, exceptSKU string) error {
	variants, err := s.cat.Variants(ctx, tx, productID)
	if err != nil {
		return err
	}
	if err := domain.CheckVariants(variants, values); err != nil {
		return err
	}
	taken, err := s.cat.CombinationTaken(ctx, tx, productID, values, exceptSKU)
	if err != nil {
		return err
	}
	if taken {
		return ycerrors.Conflict("Another SKU of the product has these variant values")
	}
	return nil
}

func anyMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
