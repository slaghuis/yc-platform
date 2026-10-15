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
	weight_grams, status, created_at, updated_at`

func scanSKU(row pgx.Row) (domain.SKU, error) {
	var s domain.SKU
	err := row.Scan(&s.ID, &s.TenantID, &s.ProductID, &s.Code, &s.VariantValues, &s.InventoryTracked,
		&s.TaxClass, &s.WeightGrams, &s.Status, &s.CreatedAt, &s.UpdatedAt)
	return s, err
}

// CreateSKU inserts a SKU of p.
func (Catalog) CreateSKU(ctx context.Context, tx pgx.Tx, p domain.Product, in domain.SKUInput) (domain.SKU, error) {
	s, err := scanSKU(tx.QueryRow(ctx, `
		INSERT INTO skus (tenant_id, product_id, sku_code, variant_values, inventory_tracked, tax_class, weight_grams, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING `+skuColumns,
		p.TenantID, p.ID, in.Code, in.VariantValues, in.InventoryTracked, in.TaxClass, in.WeightGrams, in.Status))
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
	s, err := scanSKU(tx.QueryRow(ctx, `
		UPDATE skus SET sku_code = $2, variant_values = $3, inventory_tracked = $4, tax_class = $5,
			weight_grams = $6, status = $7, updated_at = NOW()
		WHERE id = $1 AND status <> 'deleted'
		RETURNING `+skuColumns,
		id, in.Code, in.VariantValues, in.InventoryTracked, in.TaxClass, in.WeightGrams, in.Status))
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
