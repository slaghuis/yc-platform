package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	"github.com/slaghuis/yc-platform/services/yc-product/internal/domain"
)

// ComponentSKU is a candidate component looked up for kit validation.
type ComponentSKU struct {
	SKUID         string
	ProductID     string
	ProductName   string
	Kind          domain.ProductKind
	IsDigital     bool
	Status        domain.Status
	ProductStatus domain.Status
}

// LockComponentSKUs locks the live SKUs among ids (and their products),
// lowest id first, and returns those found with product facts.
func (Catalog) LockComponentSKUs(ctx context.Context, tx pgx.Tx, ids []string) (map[string]ComponentSKU, error) {
	rows, err := tx.Query(ctx, `
		SELECT s.id::text, s.product_id::text, p.name, p.kind, p.is_digital, s.status, p.status
		FROM skus s JOIN products p ON p.id = s.product_id
		WHERE s.id = ANY($1::uuid[]) AND s.status <> 'deleted' AND p.status <> 'deleted'
		ORDER BY s.id
		FOR NO KEY UPDATE OF s, p`, ids)
	if err != nil {
		return nil, fmt.Errorf("lock component skus: %w", err)
	}
	defer rows.Close()
	out := map[string]ComponentSKU{}
	for rows.Next() {
		var c ComponentSKU
		if err := rows.Scan(&c.SKUID, &c.ProductID, &c.ProductName, &c.Kind, &c.IsDigital, &c.Status, &c.ProductStatus); err != nil {
			return nil, fmt.Errorf("lock component skus: %w", err)
		}
		out[c.SKUID] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lock component skus: %w", err)
	}
	return out, nil
}

// SetBundleComponents replaces bundleSKU's component list with comps, in order.
func (Catalog) SetBundleComponents(ctx context.Context, tx pgx.Tx, tenantID, bundleSKU string, comps []domain.BundleComponentInput) error {
	if _, err := tx.Exec(ctx, `DELETE FROM bundle_sku_components WHERE bundle_sku_id = $1`, bundleSKU); err != nil {
		return fmt.Errorf("clear bundle components: %w", err)
	}
	if len(comps) == 0 {
		return nil
	}
	skuIDs := make([]string, len(comps))
	qtys := make([]int, len(comps))
	for i, c := range comps {
		skuIDs[i] = c.SKUID
		qtys[i] = c.Quantity
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO bundle_sku_components (tenant_id, bundle_sku_id, component_sku_id, quantity, sort_order)
		SELECT $1, $2, t.sku_id, t.qty, (t.ord - 1)::int
		FROM unnest($3::uuid[], $4::int[]) WITH ORDINALITY AS t(sku_id, qty, ord)`,
		tenantID, bundleSKU, skuIDs, qtys)
	if err != nil {
		return fmt.Errorf("set bundle components: %w", err)
	}
	return nil
}

// BundleComponentsOfProduct returns the ordered components of a kit product's
// (first live) SKU. Empty when the product has no SKU or no components.
func (c Catalog) BundleComponentsOfProduct(ctx context.Context, tx pgx.Tx, productID string) ([]domain.BundleComponent, error) {
	var skuID string
	err := tx.QueryRow(ctx, `SELECT id::text FROM skus
		WHERE product_id = $1 AND status <> 'deleted' ORDER BY created_at, id LIMIT 1`, productID).Scan(&skuID)
	if dbkit.IsNoRows(err) {
		return []domain.BundleComponent{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("bundle sku: %w", err)
	}
	return c.BundleComponents(ctx, tx, skuID)
}

// BundleComponents returns the ordered components of a bundle SKU.
func (Catalog) BundleComponents(ctx context.Context, tx pgx.Tx, bundleSKU string) ([]domain.BundleComponent, error) {
	rows, err := tx.Query(ctx, `
		SELECT b.component_sku_id::text, s.product_id::text, p.name, b.quantity, b.sort_order
		FROM bundle_sku_components b
		JOIN skus s ON s.id = b.component_sku_id
		JOIN products p ON p.id = s.product_id
		WHERE b.bundle_sku_id = $1
		ORDER BY b.sort_order, b.component_sku_id`, bundleSKU)
	if err != nil {
		return nil, fmt.Errorf("bundle components: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.BundleComponent, error) {
		var c domain.BundleComponent
		err := r.Scan(&c.SKUID, &c.ProductID, &c.ProductName, &c.Quantity, &c.SortOrder)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("bundle components: %w", err)
	}
	if out == nil {
		out = []domain.BundleComponent{}
	}
	return out, nil
}

// BundleComponentsOfSKUs returns components keyed by bundle SKU id.
func (Catalog) BundleComponentsOfSKUs(ctx context.Context, tx pgx.Tx, skuIDs []string) (map[string][]domain.BundleComponent, error) {
	if len(skuIDs) == 0 {
		return map[string][]domain.BundleComponent{}, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT b.bundle_sku_id::text, b.component_sku_id::text, s.product_id::text, p.name, b.quantity, b.sort_order
		FROM bundle_sku_components b
		JOIN skus s ON s.id = b.component_sku_id
		JOIN products p ON p.id = s.product_id
		WHERE b.bundle_sku_id = ANY($1::uuid[])
		ORDER BY b.bundle_sku_id, b.sort_order, b.component_sku_id`, skuIDs)
	if err != nil {
		return nil, fmt.Errorf("bundle components batch: %w", err)
	}
	defer rows.Close()
	out := map[string][]domain.BundleComponent{}
	for rows.Next() {
		var bundleID string
		var c domain.BundleComponent
		if err := rows.Scan(&bundleID, &c.SKUID, &c.ProductID, &c.ProductName, &c.Quantity, &c.SortOrder); err != nil {
			return nil, fmt.Errorf("bundle components batch: %w", err)
		}
		out[bundleID] = append(out[bundleID], c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("bundle components batch: %w", err)
	}
	return out, nil
}

// BundleProductsUsingComponent returns live kit product ids that include
// componentSKU (or any SKU of componentProduct when componentSKU is empty).
func (Catalog) BundleProductsUsingComponent(ctx context.Context, tx pgx.Tx, componentProductID, componentSKUID string) ([]string, error) {
	var rows pgx.Rows
	var err error
	switch {
	case componentSKUID != "":
		rows, err = tx.Query(ctx, `
			SELECT s.product_id::text
			FROM bundle_sku_components b
			JOIN skus s ON s.id = b.bundle_sku_id
			JOIN products p ON p.id = s.product_id
			WHERE b.component_sku_id = $1 AND p.status <> 'deleted' AND p.kind = 'bundle'
			GROUP BY s.product_id
			ORDER BY s.product_id`, componentSKUID)
	default:
		rows, err = tx.Query(ctx, `
			SELECT bs.product_id::text
			FROM bundle_sku_components b
			JOIN skus cs ON cs.id = b.component_sku_id
			JOIN skus bs ON bs.id = b.bundle_sku_id
			JOIN products p ON p.id = bs.product_id
			WHERE cs.product_id = $1 AND p.status <> 'deleted' AND p.kind = 'bundle'
			GROUP BY bs.product_id
			ORDER BY bs.product_id`, componentProductID)
	}
	if err != nil {
		return nil, fmt.Errorf("bundles using component: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("bundles using component: %w", err)
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, nil
}

// RecomputeBundleInStock sets each listed bundle SKU's projection in_stock to
// the AND of its tracked components (untracked = OK; unknown stock = OK).
func (Catalog) RecomputeBundleInStock(ctx context.Context, tx pgx.Tx, bundleSKUIDs ...string) error {
	if len(bundleSKUIDs) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		UPDATE sku_search_projection sp SET in_stock = COALESCE((
			SELECT BOOL_AND(
				CASE WHEN cs.inventory_tracked THEN COALESCE(csp.in_stock, true) ELSE true END
			)
			FROM bundle_sku_components b
			JOIN skus cs ON cs.id = b.component_sku_id
			LEFT JOIN sku_search_projection csp ON csp.sku_id = cs.id
			WHERE b.bundle_sku_id = sp.sku_id
		), true), updated_at = NOW()
		WHERE sp.sku_id = ANY($1::uuid[])`, bundleSKUIDs)
	if err != nil {
		return fmt.Errorf("recompute bundle in_stock: %w", err)
	}
	return nil
}

// BundleSKUsUsingComponent returns live bundle SKU ids that include componentSKU.
func (Catalog) BundleSKUsUsingComponent(ctx context.Context, tx pgx.Tx, componentSKU string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT b.bundle_sku_id::text
		FROM bundle_sku_components b
		JOIN skus s ON s.id = b.bundle_sku_id
		WHERE b.component_sku_id = $1 AND s.status <> 'deleted'`, componentSKU)
	if err != nil {
		return nil, fmt.Errorf("bundle skus using component: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("bundle skus using component: %w", err)
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, nil
}

// UpdateBundleProduct patches a kit product's name / description / categories.
func (c Catalog) UpdateBundleProduct(ctx context.Context, tx pgx.Tx, id string, name string, description *string, categoryIDs *[]string) (domain.Product, error) {
	p, err := c.GetProduct(ctx, tx, id, true)
	if err != nil {
		return domain.Product{}, err
	}
	if p.Kind != domain.KindBundle {
		return domain.Product{}, ycerrors.Validation().Field("product_id", "invalid", "Not a bundle product").Build()
	}
	if name == "" {
		name = p.Name
	}
	desc := p.Description
	if description != nil {
		desc = *description
	}
	p, err = scanProduct(tx.QueryRow(ctx, `
		UPDATE products SET name = $2, description = $3, updated_at = NOW()
		WHERE id = $1 AND status <> 'deleted' AND kind = 'bundle'
		RETURNING `+productColumns, id, name, desc))
	if err != nil {
		return domain.Product{}, fmt.Errorf("update bundle product: %w", err)
	}
	if categoryIDs != nil {
		if _, err := tx.Exec(ctx, `DELETE FROM product_categories WHERE product_id = $1`, id); err != nil {
			return domain.Product{}, fmt.Errorf("update bundle categories: %w", err)
		}
		if err := c.setCategories(ctx, tx, p.TenantID, id, *categoryIDs); err != nil {
			return domain.Product{}, err
		}
		p.CategoryIDs = *categoryIDs
	} else {
		p.CategoryIDs, err = c.categoryIDs(ctx, tx, id)
		if err != nil {
			return domain.Product{}, err
		}
	}
	return p, nil
}
