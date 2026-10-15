package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	"github.com/slaghuis/yc-platform/services/yc-product/internal/domain"
)

const productColumns = `id, tenant_id, COALESCE(supplier_id::text, ''), name, description, is_digital,
	status, attributes, created_at, updated_at`

func scanProduct(row pgx.Row) (domain.Product, error) {
	var p domain.Product
	err := row.Scan(&p.ID, &p.TenantID, &p.SupplierID, &p.Name, &p.Description, &p.IsDigital,
		&p.Status, &p.Attributes, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

// CreateProduct inserts a product and its categories for tenantID.
func (c Catalog) CreateProduct(ctx context.Context, tx pgx.Tx, tenantID string, in domain.ProductInput) (domain.Product, error) {
	p, err := scanProduct(tx.QueryRow(ctx, `
		INSERT INTO products (tenant_id, supplier_id, name, description, is_digital, status, attributes)
		VALUES ($1, NULLIF($2, '')::uuid, $3, $4, $5, $6, $7)
		RETURNING `+productColumns,
		tenantID, in.SupplierID, in.Name, in.Description, in.IsDigital, in.Status, in.Attributes))
	if dbkit.IsForeignKeyViolation(err) {
		return domain.Product{}, unknownSupplier()
	}
	if err != nil {
		return domain.Product{}, fmt.Errorf("create product: %w", err)
	}
	if err := c.setCategories(ctx, tx, tenantID, p.ID, in.CategoryIDs); err != nil {
		return domain.Product{}, err
	}
	p.CategoryIDs = in.CategoryIDs
	return p, nil
}

// GetProduct returns a product without its children, locked when
// forUpdate. Deleted products are not found.
func (Catalog) GetProduct(ctx context.Context, tx pgx.Tx, id string, forUpdate bool) (domain.Product, error) {
	q := `SELECT ` + productColumns + ` FROM products WHERE id = $1 AND status <> 'deleted'`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	p, err := scanProduct(tx.QueryRow(ctx, q, id))
	if dbkit.IsNoRows(err) {
		return domain.Product{}, ycerrors.NotFound("Product", id)
	}
	if err != nil {
		return domain.Product{}, fmt.Errorf("get product: %w", err)
	}
	return p, nil
}

// LoadChildren fills p's categories, variants, images and SKUs; inactive
// SKUs only when includeInactive.
func (c Catalog) LoadChildren(ctx context.Context, tx pgx.Tx, p *domain.Product, includeInactive bool) error {
	var err error
	if p.CategoryIDs, err = c.categoryIDs(ctx, tx, p.ID); err != nil {
		return err
	}
	if p.Variants, err = c.Variants(ctx, tx, p.ID); err != nil {
		return err
	}
	images, err := c.Images(ctx, tx, []string{p.ID})
	if err != nil {
		return err
	}
	p.Images = images[p.ID]
	if p.Images == nil {
		p.Images = []domain.Image{}
	}
	skus, err := c.SKUsOf(ctx, tx, []string{p.ID}, includeInactive)
	if err != nil {
		return err
	}
	p.SKUs = skus[p.ID]
	if p.SKUs == nil {
		p.SKUs = []domain.SKU{}
	}
	return nil
}

// UpdateProduct replaces a product's settable fields and categories.
func (c Catalog) UpdateProduct(ctx context.Context, tx pgx.Tx, id string, in domain.ProductInput) (domain.Product, error) {
	p, err := scanProduct(tx.QueryRow(ctx, `
		UPDATE products SET supplier_id = NULLIF($2, '')::uuid, name = $3, description = $4,
			is_digital = $5, status = $6, attributes = $7, updated_at = NOW()
		WHERE id = $1 AND status <> 'deleted'
		RETURNING `+productColumns,
		id, in.SupplierID, in.Name, in.Description, in.IsDigital, in.Status, in.Attributes))
	if dbkit.IsNoRows(err) {
		return domain.Product{}, ycerrors.NotFound("Product", id)
	}
	if dbkit.IsForeignKeyViolation(err) {
		return domain.Product{}, unknownSupplier()
	}
	if err != nil {
		return domain.Product{}, fmt.Errorf("update product: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM product_categories WHERE product_id = $1`, id); err != nil {
		return domain.Product{}, fmt.Errorf("update product categories: %w", err)
	}
	if err := c.setCategories(ctx, tx, p.TenantID, id, in.CategoryIDs); err != nil {
		return domain.Product{}, err
	}
	p.CategoryIDs = in.CategoryIDs
	return p, nil
}

// SoftDeleteProduct marks a product and its SKUs deleted and returns the
// SKUs it deleted.
func (Catalog) SoftDeleteProduct(ctx context.Context, tx pgx.Tx, id string) ([]string, error) {
	tag, err := tx.Exec(ctx, `UPDATE products SET status = 'deleted', updated_at = NOW()
		WHERE id = $1 AND status <> 'deleted'`, id)
	if err != nil {
		return nil, fmt.Errorf("delete product: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ycerrors.NotFound("Product", id)
	}
	rows, err := tx.Query(ctx, `UPDATE skus SET status = 'deleted', updated_at = NOW()
		WHERE product_id = $1 AND status <> 'deleted' RETURNING id`, id)
	if err != nil {
		return nil, fmt.Errorf("delete product skus: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("delete product skus: %w", err)
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, nil
}

func (Catalog) setCategories(ctx context.Context, tx pgx.Tx, tenantID, productID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO product_categories (tenant_id, product_id, category_id)
		SELECT $1, $2, unnest($3::uuid[])`, tenantID, productID, ids)
	if dbkit.IsForeignKeyViolation(err) {
		return ycerrors.Validation().Field("category_ids", "unknown", "Unknown category").Build()
	}
	if err != nil {
		return fmt.Errorf("set product categories: %w", err)
	}
	return nil
}

func (Catalog) categoryIDs(ctx context.Context, tx pgx.Tx, productID string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT category_id::text FROM product_categories
		WHERE product_id = $1 ORDER BY category_id`, productID)
	if err != nil {
		return nil, fmt.Errorf("product categories: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("product categories: %w", err)
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, nil
}

// AddVariant adds a variant axis to a product.
func (Catalog) AddVariant(ctx context.Context, tx pgx.Tx, p domain.Product, in domain.VariantInput) (domain.Variant, error) {
	var va domain.Variant
	err := tx.QueryRow(ctx, `
		INSERT INTO product_variants (tenant_id, product_id, axis_name, values) VALUES ($1, $2, $3, $4)
		RETURNING id, product_id, axis_name, values, created_at`,
		p.TenantID, p.ID, in.AxisName, in.Values).
		Scan(&va.ID, &va.ProductID, &va.AxisName, &va.Values, &va.CreatedAt)
	if dbkit.IsUniqueViolation(err) {
		return domain.Variant{}, ycerrors.Conflict("The product already has a " + in.AxisName + " axis")
	}
	if err != nil {
		return domain.Variant{}, fmt.Errorf("add variant: %w", err)
	}
	return va, nil
}

// Variants returns a product's variant axes in creation order.
func (Catalog) Variants(ctx context.Context, tx pgx.Tx, productID string) ([]domain.Variant, error) {
	rows, err := tx.Query(ctx, `SELECT id, product_id, axis_name, values, created_at
		FROM product_variants WHERE product_id = $1 ORDER BY created_at, axis_name`, productID)
	if err != nil {
		return nil, fmt.Errorf("variants: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Variant, error) {
		var va domain.Variant
		err := r.Scan(&va.ID, &va.ProductID, &va.AxisName, &va.Values, &va.CreatedAt)
		return va, err
	})
	if err != nil {
		return nil, fmt.Errorf("variants: %w", err)
	}
	if out == nil {
		out = []domain.Variant{}
	}
	return out, nil
}

// AttachImage attaches or re-attaches an image. A primary image demotes the
// product's previous primary.
func (Catalog) AttachImage(ctx context.Context, tx pgx.Tx, p domain.Product, img domain.Image) error {
	if img.IsPrimary {
		if _, err := tx.Exec(ctx, `UPDATE product_images SET is_primary = FALSE
			WHERE product_id = $1 AND is_primary AND image_id <> $2`, p.ID, img.ImageID); err != nil {
			return fmt.Errorf("demote primary image: %w", err)
		}
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO product_images (tenant_id, product_id, image_id, sort_order, is_primary)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (product_id, image_id) DO UPDATE SET sort_order = EXCLUDED.sort_order, is_primary = EXCLUDED.is_primary`,
		p.TenantID, p.ID, img.ImageID, img.SortOrder, img.IsPrimary)
	if err != nil {
		return fmt.Errorf("attach image: %w", err)
	}
	return nil
}

// DetachImage removes an image from a product.
func (Catalog) DetachImage(ctx context.Context, tx pgx.Tx, productID, imageID string) error {
	tag, err := tx.Exec(ctx, `DELETE FROM product_images WHERE product_id = $1 AND image_id = $2`, productID, imageID)
	if err != nil {
		return fmt.Errorf("detach image: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ycerrors.NotFound("Image", imageID)
	}
	return nil
}

// Images returns the images of products by product ID, primary first,
// then by sort order.
func (Catalog) Images(ctx context.Context, tx pgx.Tx, productIDs []string) (map[string][]domain.Image, error) {
	rows, err := tx.Query(ctx, `SELECT product_id::text, image_id::text, sort_order, is_primary FROM product_images
		WHERE product_id = ANY($1::uuid[]) ORDER BY is_primary DESC, sort_order, created_at`, productIDs)
	if err != nil {
		return nil, fmt.Errorf("images: %w", err)
	}
	defer rows.Close()
	out := map[string][]domain.Image{}
	for rows.Next() {
		var pid string
		var img domain.Image
		if err := rows.Scan(&pid, &img.ImageID, &img.SortOrder, &img.IsPrimary); err != nil {
			return nil, fmt.Errorf("images: %w", err)
		}
		out[pid] = append(out[pid], img)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("images: %w", err)
	}
	return out, nil
}

func unknownSupplier() error {
	return ycerrors.Validation().Field("supplier_id", "unknown", "Unknown supplier").Build()
}
