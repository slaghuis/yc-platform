package repository

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/slaghuis/yc-platform/services/yc-product/internal/domain"
)

// RefreshProjection rewrites the sku_search_projection rows of products
// from the catalog tables. Columns fed by other services' events (price,
// stock, popularity) are kept, except that an untracked SKU is always in
// stock.
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
			in_stock = CASE WHEN (SELECT inventory_tracked FROM skus WHERE id = EXCLUDED.sku_id) THEN sp.in_stock END,
			updated_at = EXCLUDED.updated_at`, productIDs)
	if err != nil {
		return fmt.Errorf("refresh search projection: %w", err)
	}
	return nil
}

// RecordOrder counts an order towards its SKUs' popularity and, when
// userID is set, towards the customer's favourite products.
func (Catalog) RecordOrder(ctx context.Context, tx pgx.Tx, tenantID, userID string, skuIDs []string, at time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE sku_search_projection SET popularity_score = popularity_score + 1
		WHERE sku_id = ANY($1::uuid[])`, skuIDs)
	if err != nil {
		return fmt.Errorf("add popularity: %w", err)
	}
	if userID == "" {
		return nil
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO customer_favourites AS f (tenant_id, user_id, product_id, order_count, last_ordered_at)
		SELECT $1, $2, product_id, 1, $4 FROM (SELECT DISTINCT product_id FROM skus WHERE id = ANY($3::uuid[])) s
		ON CONFLICT (tenant_id, user_id, product_id) DO UPDATE SET
			order_count = f.order_count + 1, last_ordered_at = GREATEST(f.last_ordered_at, EXCLUDED.last_ordered_at)`,
		tenantID, userID, skuIDs, at)
	if err != nil {
		return fmt.Errorf("record favourites: %w", err)
	}
	return nil
}

// SetReviewStats records a product's review stats unless a newer version
// is already recorded.
func (Catalog) SetReviewStats(ctx context.Context, tx pgx.Tx, tenantID, productID string, s domain.ReviewStats) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO product_review_stats AS r (tenant_id, product_id, rating_count, rating_sum, like_count, version)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (product_id) DO UPDATE SET
			rating_count = EXCLUDED.rating_count, rating_sum = EXCLUDED.rating_sum,
			like_count = EXCLUDED.like_count, version = EXCLUDED.version, updated_at = NOW()
		WHERE r.version < EXCLUDED.version`,
		tenantID, productID, s.RatingCount, s.RatingSum, s.LikeCount, s.Version)
	if err != nil {
		return fmt.Errorf("set review stats: %w", err)
	}
	return nil
}

// DeleteFavourites removes userID's favourites, returning how many went.
func (Catalog) DeleteFavourites(ctx context.Context, tx pgx.Tx, tenantID, userID string) (int64, error) {
	tag, err := tx.Exec(ctx, `DELETE FROM customer_favourites WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID)
	if err != nil {
		return 0, fmt.Errorf("delete favourites: %w", err)
	}
	return tag.RowsAffected(), nil
}

// FavouriteCount counts the active products userID has ordered.
func (Catalog) FavouriteCount(ctx context.Context, tx pgx.Tx, userID string) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM customer_favourites f JOIN products p ON p.id = f.product_id
		WHERE f.user_id = $1 AND p.status = 'active' AND EXISTS (
			SELECT 1 FROM sku_search_projection sp WHERE sp.product_id = p.id AND sp.status = 'active')`, userID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count favourites: %w", err)
	}
	return n, nil
}

// SetBasePrice records a SKU's base price in minor units (nil when
// removed) as of at, unless a later change is already recorded.
func (Catalog) SetBasePrice(ctx context.Context, tx pgx.Tx, skuID string, minor *int64, at time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE sku_search_projection SET base_price = $2, base_price_at = $3
		WHERE sku_id = $1 AND (base_price_at IS NULL OR base_price_at <= $3)`, skuID, minor, at)
	if err != nil {
		return fmt.Errorf("set base price: %w", err)
	}
	return nil
}

// SetInStock records whether a tracked SKU has stock as of at, unless a
// later change is already recorded.
func (Catalog) SetInStock(ctx context.Context, tx pgx.Tx, skuID string, inStock bool, at time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE sku_search_projection SET in_stock = $2, in_stock_at = $3
		WHERE sku_id = $1 AND (in_stock_at IS NULL OR in_stock_at <= $3)`, skuID, inStock, at)
	if err != nil {
		return fmt.Errorf("set in stock: %w", err)
	}
	return nil
}

// priceKey sorts unpriced products last in either direction.
const (
	unpricedAsc  = math.MaxInt64
	unpricedDesc = -1
)

// SortKey is p's cursor key under sort.
func SortKey(sort domain.Sort, p domain.ProductSummary) int64 {
	switch sort {
	case domain.SortPopular:
		return p.Popularity
	case domain.SortPriceAsc:
		if p.MinBasePrice == nil {
			return unpricedAsc
		}
		return *p.MinBasePrice
	case domain.SortPriceDesc:
		if p.MinBasePrice == nil {
			return unpricedDesc
		}
		return *p.MinBasePrice
	case domain.SortRating:
		return p.Reviews.RatingKey()
	case domain.SortNewest:
	}
	return 0
}

// Browse returns up to limit active products after the cursor, in
// f.Sort order (newest when empty), that have an active SKU matching f.
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
	if f.InStock {
		sku = append(sku, `COALESCE(sp.in_stock, true)`)
	}
	where := []string{
		`p.status = 'active'`,
		`EXISTS (SELECT 1 FROM sku_search_projection sp WHERE sp.product_id = p.id AND sp.status = 'active'` +
			prefixAnd(sku) + `)`,
	}
	if f.SupplierID != "" {
		where = append(where, `p.supplier_id = `+arg(f.SupplierID)+`::uuid`)
	}
	if f.FavouritesOf != "" {
		where = append(where, `EXISTS (SELECT 1 FROM customer_favourites f WHERE f.product_id = p.id AND f.user_id = `+
			arg(f.FavouritesOf)+`::uuid)`)
	}

	var order, cursor string
	switch f.Sort {
	case domain.SortPopular:
		order = `b.popularity DESC, b.created_at DESC, b.id DESC`
		if after != nil {
			cursor = fmt.Sprintf(`(b.popularity, b.created_at, b.id) < (%s::bigint, %s, %s::uuid)`,
				arg(after.Key), arg(after.CreatedAt), arg(after.ID))
		}
	case domain.SortPriceAsc:
		key := fmt.Sprintf(`COALESCE(b.min_price, %d)`, unpricedAsc)
		order = key + ` ASC, b.id ASC`
		if after != nil {
			cursor = fmt.Sprintf(`(%s, b.id) > (%s::bigint, %s::uuid)`, key, arg(after.Key), arg(after.ID))
		}
	case domain.SortPriceDesc:
		key := fmt.Sprintf(`COALESCE(b.min_price, %d)`, unpricedDesc)
		order = key + ` DESC, b.id DESC`
		if after != nil {
			cursor = fmt.Sprintf(`(%s, b.id) < (%s::bigint, %s::uuid)`, key, arg(after.Key), arg(after.ID))
		}
	case domain.SortRating:
		// Matches domain.ReviewStats.RatingKey.
		key := `(CASE WHEN b.rating_count > 0 THEN b.rating_sum * 1000 / b.rating_count ELSE -1 END)`
		order = key + ` DESC, b.created_at DESC, b.id DESC`
		if after != nil {
			cursor = fmt.Sprintf(`(%s, b.created_at, b.id) < (%s::bigint, %s, %s::uuid)`,
				key, arg(after.Key), arg(after.CreatedAt), arg(after.ID))
		}
	case domain.SortNewest, "":
		order = `b.created_at DESC, b.id DESC`
		if after != nil {
			cursor = fmt.Sprintf(`(b.created_at, b.id) < (%s, %s::uuid)`, arg(after.CreatedAt), arg(after.ID))
		}
	}
	if cursor != "" {
		cursor = ` WHERE ` + cursor
	}
	q := `SELECT b.id, b.supplier_id, b.name, b.description, b.is_digital, b.created_at, b.category_ids,
			b.popularity, b.min_price, b.in_stock, b.rating_count, b.rating_sum, b.like_count
		FROM (SELECT p.id, COALESCE(p.supplier_id::text, '') AS supplier_id, p.name, p.description, p.is_digital,
				p.created_at,
				COALESCE(r.rating_count, 0)::bigint AS rating_count, COALESCE(r.rating_sum, 0)::bigint AS rating_sum,
				COALESCE(r.like_count, 0)::bigint AS like_count,
				EXISTS (SELECT 1 FROM sku_search_projection sp WHERE sp.product_id = p.id
					AND sp.status = 'active' AND COALESCE(sp.in_stock, true)) AS in_stock,
				COALESCE((SELECT array_agg(pc.category_id::text ORDER BY pc.category_id)
					FROM product_categories pc WHERE pc.product_id = p.id), '{}') AS category_ids,
				COALESCE((SELECT SUM(sp.popularity_score) FROM sku_search_projection sp
					WHERE sp.product_id = p.id AND sp.status = 'active'), 0)::bigint AS popularity,
				(SELECT MIN(sp.base_price) FROM sku_search_projection sp
					WHERE sp.product_id = p.id AND sp.status = 'active') AS min_price
			FROM products p LEFT JOIN product_review_stats r ON r.product_id = p.id
			WHERE ` + strings.Join(where, " AND ") + `) b` + cursor +
		fmt.Sprintf(` ORDER BY %s LIMIT %d`, order, limit)

	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("browse: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.ProductSummary, error) {
		var p domain.ProductSummary
		err := r.Scan(&p.ID, &p.SupplierID, &p.Name, &p.Description, &p.IsDigital, &p.CreatedAt, &p.CategoryIDs,
			&p.Popularity, &p.MinBasePrice, &p.InStock, &p.Reviews.RatingCount, &p.Reviews.RatingSum, &p.Reviews.LikeCount)
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
