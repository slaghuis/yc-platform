package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	"github.com/slaghuis/yc-platform/services/yc-product/internal/domain"
)

const skuColumns = `id, tenant_id, product_id, sku_code, variant_values, inventory_tracked, tax_class,
	weight_grams, length_mm, width_mm, height_mm, status, created_at, updated_at`

// dimensions scans the all-or-none length_mm, width_mm and height_mm.
type dimensions struct{ l, w, h *int }

func (d *dimensions) dest() []any { return []any{&d.l, &d.w, &d.h} }

func (d dimensions) value() *domain.Dimensions {
	if d.l == nil || d.w == nil || d.h == nil {
		return nil
	}
	return &domain.Dimensions{LengthMM: *d.l, WidthMM: *d.w, HeightMM: *d.h}
}

// dimensionArgs are the length_mm, width_mm and height_mm to write.
func dimensionArgs(d *domain.Dimensions) (l, w, h *int) {
	if d == nil {
		return nil, nil, nil
	}
	return &d.LengthMM, &d.WidthMM, &d.HeightMM
}

func scanSKU(row pgx.Row) (domain.SKU, error) {
	var s domain.SKU
	var d dimensions
	dest := append([]any{
		&s.ID, &s.TenantID, &s.ProductID, &s.Code, &s.VariantValues, &s.InventoryTracked,
		&s.TaxClass, &s.WeightGrams,
	}, d.dest()...)
	err := row.Scan(append(dest, &s.Status, &s.CreatedAt, &s.UpdatedAt)...)
	s.Dimensions = d.value()
	return s, err
}

// SKUDetails returns the SKUs with ids, deleted ones included, with their
// product's name, kind, digital flag and (for kits) component expansion.
func (c Catalog) SKUDetails(ctx context.Context, tx pgx.Tx, ids []string) ([]domain.SKUDetail, error) {
	rows, err := tx.Query(ctx, `
		SELECT s.id, s.tenant_id, s.product_id, s.sku_code, s.variant_values, s.inventory_tracked, s.tax_class,
			s.weight_grams, s.length_mm, s.width_mm, s.height_mm, CASE WHEN p.status = 'deleted' THEN 'deleted' ELSE s.status END, s.created_at, s.updated_at,
			p.name, p.is_digital, p.kind, p.status = 'active' AND s.status = 'active'
		FROM skus s JOIN products p ON p.id = s.product_id
		WHERE s.id = ANY($1::uuid[])
		ORDER BY s.id`, ids)
	if err != nil {
		return nil, fmt.Errorf("sku details: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.SKUDetail, error) {
		var d domain.SKUDetail
		var dims dimensions
		s := &d.SKU
		dest := append([]any{
			&s.ID, &s.TenantID, &s.ProductID, &s.Code, &s.VariantValues, &s.InventoryTracked,
			&s.TaxClass, &s.WeightGrams,
		}, dims.dest()...)
		err := r.Scan(append(dest, &s.Status, &s.CreatedAt, &s.UpdatedAt, &d.ProductName, &d.IsDigital, &d.Kind, &d.Active)...)
		s.Dimensions = dims.value()
		d.Components = []domain.BundleComponent{}
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("sku details: %w", err)
	}
	bundleIDs := make([]string, 0, len(out))
	for _, d := range out {
		if d.Kind == domain.KindBundle {
			bundleIDs = append(bundleIDs, d.ID)
		}
	}
	comps, err := c.BundleComponentsOfSKUs(ctx, tx, bundleIDs)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if c, ok := comps[out[i].ID]; ok {
			out[i].Components = c
		}
	}
	return out, nil
}

// CreateSKU inserts a SKU of p.
func (Catalog) CreateSKU(ctx context.Context, tx pgx.Tx, p domain.Product, in domain.SKUInput) (domain.SKU, error) {
	l, w, h := dimensionArgs(in.Dimensions)
	s, err := scanSKU(tx.QueryRow(ctx, `
		INSERT INTO skus (tenant_id, product_id, sku_code, variant_values, inventory_tracked, tax_class, weight_grams,
			status, length_mm, width_mm, height_mm)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING `+skuColumns,
		p.TenantID, p.ID, in.Code, in.VariantValues, in.InventoryTracked, in.TaxClass, in.WeightGrams, in.Status, l, w, h))
	if dbkit.IsUniqueViolation(err) {
		return domain.SKU{}, skuCodeTaken(in.Code)
	}
	if err != nil {
		return domain.SKU{}, fmt.Errorf("create sku: %w", err)
	}
	return s, nil
}

// GetSKU returns a SKU, locked when forUpdate. Deleted SKUs are not found.
func (Catalog) GetSKU(ctx context.Context, tx pgx.Tx, id string, forUpdate bool) (domain.SKU, error) {
	q := `SELECT ` + skuColumns + ` FROM skus WHERE id = $1 AND status <> 'deleted'`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	s, err := scanSKU(tx.QueryRow(ctx, q, id))
	if dbkit.IsNoRows(err) {
		return domain.SKU{}, ycerrors.NotFound("SKU", id)
	}
	if err != nil {
		return domain.SKU{}, fmt.Errorf("get sku: %w", err)
	}
	return s, nil
}

// UpdateSKU replaces a SKU's settable fields.
func (Catalog) UpdateSKU(ctx context.Context, tx pgx.Tx, id string, in domain.SKUInput) (domain.SKU, error) {
	l, w, h := dimensionArgs(in.Dimensions)
	s, err := scanSKU(tx.QueryRow(ctx, `
		UPDATE skus SET sku_code = $2, variant_values = $3, inventory_tracked = $4, tax_class = $5,
			weight_grams = $6, status = $7, length_mm = $8, width_mm = $9, height_mm = $10, updated_at = NOW()
		WHERE id = $1 AND status <> 'deleted'
		RETURNING `+skuColumns,
		id, in.Code, in.VariantValues, in.InventoryTracked, in.TaxClass, in.WeightGrams, in.Status, l, w, h))
	if dbkit.IsNoRows(err) {
		return domain.SKU{}, ycerrors.NotFound("SKU", id)
	}
	if dbkit.IsUniqueViolation(err) {
		return domain.SKU{}, skuCodeTaken(in.Code)
	}
	if err != nil {
		return domain.SKU{}, fmt.Errorf("update sku: %w", err)
	}
	return s, nil
}

// SoftDeleteSKU marks a SKU deleted.
func (Catalog) SoftDeleteSKU(ctx context.Context, tx pgx.Tx, id string) error {
	tag, err := tx.Exec(ctx, `UPDATE skus SET status = 'deleted', updated_at = NOW()
		WHERE id = $1 AND status <> 'deleted'`, id)
	if err != nil {
		return fmt.Errorf("delete sku: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ycerrors.NotFound("SKU", id)
	}
	return nil
}

// CountSKUs returns how many SKUs of a product are not deleted.
func (Catalog) CountSKUs(ctx context.Context, tx pgx.Tx, productID string) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM skus WHERE product_id = $1 AND status <> 'deleted'`, productID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count skus: %w", err)
	}
	return n, nil
}

// CombinationTaken reports whether another live SKU of the product has the
// same variant values.
func (Catalog) CombinationTaken(ctx context.Context, tx pgx.Tx, productID string, values map[string]string, exceptSKU string) (bool, error) {
	var taken bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM skus WHERE product_id = $1 AND status <> 'deleted'
			AND variant_values = $2::jsonb AND id::text <> $3)`,
		productID, values, exceptSKU).Scan(&taken)
	if err != nil {
		return false, fmt.Errorf("sku combination: %w", err)
	}
	return taken, nil
}

// SKUsOf returns the live SKUs of products by product ID, by code; only
// active ones unless includeInactive.
func (Catalog) SKUsOf(ctx context.Context, tx pgx.Tx, productIDs []string, includeInactive bool) (map[string][]domain.SKU, error) {
	q := `SELECT ` + skuColumns + ` FROM skus WHERE product_id = ANY($1::uuid[])`
	if includeInactive {
		q += ` AND status <> 'deleted'`
	} else {
		q += ` AND status = 'active'`
	}
	rows, err := tx.Query(ctx, q+` ORDER BY sku_code`, productIDs)
	if err != nil {
		return nil, fmt.Errorf("skus: %w", err)
	}
	skus, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.SKU, error) { return scanSKU(r) })
	if err != nil {
		return nil, fmt.Errorf("skus: %w", err)
	}
	out := map[string][]domain.SKU{}
	for _, s := range skus {
		out[s.ProductID] = append(out[s.ProductID], s)
	}
	return out, nil
}

func skuCodeTaken(code string) error {
	return ycerrors.Conflict("SKU code " + code + " is already in use")
}
