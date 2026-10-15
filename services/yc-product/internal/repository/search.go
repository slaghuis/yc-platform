package repository

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/slaghuis/yc-platform/services/yc-product/internal/domain"
)

// RefreshProjection rewrites the sku_search_projection rows of products
// from the catalog tables. Columns fed by other services' events (price,
// stock, popularity, rating) are kept.
func (Catalog) RefreshProjection(ctx context.Context, tx pgx.Tx, productIDs ...string) error {
	if len(productIDs) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO sku_search_projection AS sp (sku_id, tenant_id, product_id, supplier_id, category_ids,
			name, description, sku_code, tsv, variant_values, status, updated_at)
		SELECT s.id, s.tenant_id, p.id, p.supplier_id,
			COALESCE((SELECT array_agg(pc.category_id ORDER BY pc.category_id)
				FROM product_categories pc WHERE pc.product_id = p.id), '{}'),
			p.name, p.description, s.sku_code,
			setweight(to_tsvector('english', p.name), 'A') ||
			setweight(to_tsvector('english', p.description), 'B') ||
			setweight(to_tsvector('simple', s.sku_code || ' ' ||
				COALESCE((SELECT string_agg(value, ' ') FROM jsonb_each_text(s.variant_values)), '')), 'C'),
			s.variant_values,
			CASE WHEN p.status = 'deleted' OR s.status = 'deleted' THEN 'deleted'
			     WHEN p.status = 'active' AND s.status = 'active' THEN 'active'
			     ELSE 'inactive' END,
			NOW()
		FROM skus s JOIN products p ON p.id = s.product_id
		WHERE p.id = ANY($1::uuid[])
		ON CONFLICT (sku_id) DO UPDATE SET
			supplier_id = EXCLUDED.supplier_id, category_ids = EXCLUDED.category_ids,
			name = EXCLUDED.name, description = EXCLUDED.description, sku_code = EXCLUDED.sku_code,
			tsv = EXCLUDED.tsv, variant_values = EXCLUDED.variant_values, status = EXCLUDED.status,
			updated_at = EXCLUDED.updated_at`, productIDs)
	if err != nil {
		return fmt.Errorf("refresh search projection: %w", err)
	}
	return nil
}

// Browse returns up to limit active products after the cursor, newest
// first, that have an active SKU matching f.
func (c Catalog) Browse(ctx context.Context, tx pgx.Tx, f domain.BrowseFilter, after *Cursor, limit int) ([]domain.ProductSummary, error) {
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	var sku []string
	if f.Query != "" {
		q := arg(f.Query)
		sku = append(sku, fmt.Sprintf(`sp.tsv @@ (websearch_to_tsquery('english', %s) || websearch_to_tsquery('simple', %s))`, q, q))
	}
	if f.CategoryID != "" {
		sku = append(sku, `sp.category_ids && ARRAY(
			WITH RECURSIVE sub AS (
				SELECT id FROM categories WHERE id = `+arg(f.CategoryID)+`::uuid
				UNION
				SELECT ch.id FROM categories ch JOIN sub ON ch.parent_id = sub.id
			) SELECT id FROM sub)`)
	}
	where := []string{
		`p.status = 'active'`,
		`EXISTS (SELECT 1 FROM sku_search_projection sp WHERE sp.product_id = p.id AND sp.status = 'active'` +
			prefixAnd(sku) + `)`,
	}
	if f.SupplierID != "" {
		where = append(where, `p.supplier_id = `+arg(f.SupplierID)+`::uuid`)
	}
	if after != nil {
		where = append(where, fmt.Sprintf(`(p.created_at, p.id) < (%s, %s::uuid)`, arg(after.CreatedAt), arg(after.ID)))
	}
	q := `SELECT p.id, COALESCE(p.supplier_id::text, ''), p.name, p.description, p.is_digital, p.created_at,
			COALESCE((SELECT array_agg(pc.category_id::text ORDER BY pc.category_id)
				FROM product_categories pc WHERE pc.product_id = p.id), '{}')
		FROM products p WHERE ` + strings.Join(where, " AND ") +
		fmt.Sprintf(` ORDER BY p.created_at DESC, p.id DESC LIMIT %d`, limit)

	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("browse: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.ProductSummary, error) {
		var p domain.ProductSummary
		err := r.Scan(&p.ID, &p.SupplierID, &p.Name, &p.Description, &p.IsDigital, &p.CreatedAt, &p.CategoryIDs)
		return p, err
	})
	if err != nil {
		return nil, fmt.Errorf("browse: %w", err)
	}
	if len(out) == 0 {
		return out, nil
	}

	ids := make([]string, len(out))
	for i, p := range out {
		ids[i] = p.ID
	}
	skus, err := c.SKUsOf(ctx, tx, ids, false)
	if err != nil {
		return nil, err
	}
	images, err := c.Images(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].SKUs = skus[out[i].ID]
		if imgs := images[out[i].ID]; len(imgs) > 0 {
			out[i].PrimaryImageID = imgs[0].ImageID
		}
	}
	return out, nil
}

func prefixAnd(conds []string) string {
	if len(conds) == 0 {
		return ""
	}
	return " AND " + strings.Join(conds, " AND ")
}
