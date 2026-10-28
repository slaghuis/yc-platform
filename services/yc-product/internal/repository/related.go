package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	"github.com/slaghuis/yc-platform/services/yc-product/internal/domain"
)

// activeProduct is true for a product p that shoppers may see: active with
// an active SKU.
const activeProduct = `p.status = 'active' AND EXISTS (
	SELECT 1 FROM sku_search_projection sp WHERE sp.product_id = p.id AND sp.status = 'active')`

// LockProducts locks the live (not deleted) products among ids, lowest id
// first so concurrent callers cannot deadlock, and returns the set of ids found. Holding
// the locks keeps a target from being soft-deleted between the caller's
// check and its write.
func (Catalog) LockProducts(ctx context.Context, tx pgx.Tx, ids []string) (map[string]bool, error) {
	rows, err := tx.Query(ctx, `SELECT id::text FROM products
		WHERE id = ANY($1::uuid[]) AND status <> 'deleted' ORDER BY id FOR NO KEY UPDATE`, ids)
	if err != nil {
		return nil, fmt.Errorf("lock products: %w", err)
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("lock products: %w", err)
	}
	out := make(map[string]bool, len(found))
	for _, id := range found {
		out[id] = true
	}
	return out, nil
}

// SetRelated replaces productID's related list with ids, in order. The
// caller has checked that every id is a live product of the shop.
func (Catalog) SetRelated(ctx context.Context, tx pgx.Tx, tenantID, productID string, ids []string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM product_related WHERE product_id = $1`, productID); err != nil {
		return fmt.Errorf("clear related products: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO product_related (tenant_id, product_id, related_product_id, sort_order)
		SELECT $1, $2, t.id, (t.ord - 1)::int FROM unnest($3::uuid[]) WITH ORDINALITY AS t(id, ord)`,
		tenantID, productID, ids)
	switch {
	case dbkit.IsForeignKeyViolation(err):
		return ycerrors.Validation().Field("product_ids", "unknown", "Unknown product").Build()
	case err != nil:
		return fmt.Errorf("set related products: %w", err)
	}
	return nil
}

// ListRelated returns productID's related product ids in display order.
// Admins get the stored list; otherwise only active targets with an active
// SKU.
func (Catalog) ListRelated(ctx context.Context, tx pgx.Tx, productID string, includeInactive bool) ([]string, error) {
	q := `SELECT r.related_product_id::text
		FROM product_related r JOIN products p ON p.id = r.related_product_id
		WHERE r.product_id = $1 AND p.status <> 'deleted'`
	if !includeInactive {
		q += ` AND ` + activeProduct
	}
	rows, err := tx.Query(ctx, q+` ORDER BY r.sort_order, r.related_product_id`, productID)
	if err != nil {
		return nil, fmt.Errorf("related products: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("related products: %w", err)
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, nil
}

// DeleteRelatedForProduct removes every related link that starts or ends
// at productID.
func (Catalog) DeleteRelatedForProduct(ctx context.Context, tx pgx.Tx, productID string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM product_related WHERE product_id = $1 OR related_product_id = $1`, productID); err != nil {
		return fmt.Errorf("delete related products: %w", err)
	}
	return nil
}

// RecordAlsoBought counts an order towards the also-bought projection: the
// products of skuIDs, each once, and for every pair of them both directions
// go up by one.
func (Catalog) RecordAlsoBought(ctx context.Context, tx pgx.Tx, tenantID string, skuIDs []string) error {
	// ORDER BY gives concurrent orders the same row order, so their upserts
	// cannot deadlock.
	_, err := tx.Exec(ctx, `
		WITH ordered AS (SELECT DISTINCT product_id FROM skus WHERE id = ANY($2::uuid[]))
		INSERT INTO product_also_bought AS ab (tenant_id, product_id, other_product_id, order_count, updated_at)
		SELECT $1, a.product_id, b.product_id, 1, NOW()
		FROM ordered a JOIN ordered b ON a.product_id <> b.product_id
		ORDER BY a.product_id, b.product_id
		ON CONFLICT (tenant_id, product_id, other_product_id) DO UPDATE SET
			order_count = ab.order_count + 1, updated_at = NOW()`,
		tenantID, skuIDs)
	if err != nil {
		return fmt.Errorf("record also bought: %w", err)
	}
	return nil
}

// AlsoBought returns up to limit active products most often ordered with
// productID, most first, ties by product id.
func (c Catalog) AlsoBought(ctx context.Context, tx pgx.Tx, productID string, limit int) ([]domain.ProductSummary, error) {
	rows, err := tx.Query(ctx, `
		SELECT ab.other_product_id::text
		FROM product_also_bought ab JOIN products p ON p.id = ab.other_product_id
		WHERE ab.product_id = $1 AND ab.other_product_id <> $1 AND `+activeProduct+`
		ORDER BY ab.order_count DESC, ab.other_product_id
		LIMIT $2`, productID, limit)
	if err != nil {
		return nil, fmt.Errorf("also bought: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("also bought: %w", err)
	}
	if len(ids) == 0 {
		return []domain.ProductSummary{}, nil
	}
	rows, err = tx.Query(ctx, summarySelect+` FROM `+summaryFrom(`p.id = ANY($1::uuid[])`), ids)
	if err != nil {
		return nil, fmt.Errorf("also bought: %w", err)
	}
	found, err := collectSummaries(rows)
	if err != nil {
		return nil, fmt.Errorf("also bought: %w", err)
	}
	byID := make(map[string]domain.ProductSummary, len(found))
	for _, p := range found {
		byID[p.ID] = p
	}
	ordered := make([]domain.ProductSummary, 0, len(ids))
	for _, id := range ids {
		if p, ok := byID[id]; ok {
			ordered = append(ordered, p)
		}
	}
	return c.hydrateSummaries(ctx, tx, ordered)
}
