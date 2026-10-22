package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	"github.com/slaghuis/yc-platform/services/yc-product/internal/domain"
)

const categoryColumns = `id, COALESCE(parent_id::text, ''), name, slug, sort_order, COALESCE(image_id::text, '')`

func scanCategory(row pgx.Row) (domain.Category, error) {
	var c domain.Category
	err := row.Scan(&c.ID, &c.ParentID, &c.Name, &c.Slug, &c.SortOrder, &c.ImageID)
	return c, err
}

// CreateCategory inserts a category for tenantID.
func (Catalog) CreateCategory(ctx context.Context, tx pgx.Tx, tenantID string, in domain.CategoryInput) (domain.Category, error) {
	c, err := scanCategory(tx.QueryRow(ctx, `
		INSERT INTO categories (tenant_id, parent_id, name, slug, sort_order, image_id)
		VALUES ($1, NULLIF($2, '')::uuid, $3, $4, $5, NULLIF($6, '')::uuid) RETURNING `+categoryColumns,
		tenantID, in.ParentID, in.Name, in.Slug, in.SortOrder, in.ImageID))
	if err != nil {
		return domain.Category{}, categoryWriteError(err, in.Slug)
	}
	return c, nil
}

// GetCategory returns a category.
func (Catalog) GetCategory(ctx context.Context, tx pgx.Tx, id string) (domain.Category, error) {
	c, err := scanCategory(tx.QueryRow(ctx, `SELECT `+categoryColumns+` FROM categories WHERE id = $1`, id))
	if dbkit.IsNoRows(err) {
		return domain.Category{}, ycerrors.NotFound("Category", id)
	}
	if err != nil {
		return domain.Category{}, fmt.Errorf("get category: %w", err)
	}
	return c, nil
}

// UpdateCategory replaces a category's fields. The caller has checked the
// new parent is not the category or a descendant.
func (Catalog) UpdateCategory(ctx context.Context, tx pgx.Tx, id string, in domain.CategoryInput) (domain.Category, error) {
	c, err := scanCategory(tx.QueryRow(ctx, `
		UPDATE categories SET parent_id = NULLIF($2, '')::uuid, name = $3, slug = $4, sort_order = $5,
			image_id = NULLIF($6, '')::uuid, updated_at = NOW()
		WHERE id = $1 RETURNING `+categoryColumns,
		id, in.ParentID, in.Name, in.Slug, in.SortOrder, in.ImageID))
	if dbkit.IsNoRows(err) {
		return domain.Category{}, ycerrors.NotFound("Category", id)
	}
	if err != nil {
		return domain.Category{}, categoryWriteError(err, in.Slug)
	}
	return c, nil
}

// IsSelfOrDescendant reports whether candidate is id or below it.
func (Catalog) IsSelfOrDescendant(ctx context.Context, tx pgx.Tx, id, candidate string) (bool, error) {
	var found bool
	err := tx.QueryRow(ctx, `
		WITH RECURSIVE sub AS (
			SELECT id FROM categories WHERE id = $1
			UNION
			SELECT c.id FROM categories c JOIN sub ON c.parent_id = sub.id
		)
		SELECT EXISTS (SELECT 1 FROM sub WHERE id = $2)`, id, candidate).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("category descendants: %w", err)
	}
	return found, nil
}

// DeleteCategory deletes a category without subcategories and returns the
// products that were in it.
func (Catalog) DeleteCategory(ctx context.Context, tx pgx.Tx, id string) ([]string, error) {
	var hasChildren bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM categories WHERE parent_id = $1)`, id).Scan(&hasChildren); err != nil {
		return nil, fmt.Errorf("delete category: %w", err)
	}
	if hasChildren {
		return nil, ycerrors.Conflict("Delete or move the subcategories first")
	}
	rows, err := tx.Query(ctx, `SELECT product_id::text FROM product_categories WHERE category_id = $1`, id)
	if err != nil {
		return nil, fmt.Errorf("delete category: %w", err)
	}
	products, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("delete category: %w", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM categories WHERE id = $1`, id)
	if err != nil {
		return nil, fmt.Errorf("delete category: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ycerrors.NotFound("Category", id)
	}
	return products, nil
}

// Categories returns every category the transaction sees.
func (Catalog) Categories(ctx context.Context, tx pgx.Tx) ([]domain.Category, error) {
	rows, err := tx.Query(ctx, `SELECT `+categoryColumns+` FROM categories`)
	if err != nil {
		return nil, fmt.Errorf("categories: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Category, error) { return scanCategory(r) })
	if err != nil {
		return nil, fmt.Errorf("categories: %w", err)
	}
	return out, nil
}

func categoryWriteError(err error, slug string) error {
	switch {
	case dbkit.IsUniqueViolation(err):
		return ycerrors.Conflict("Slug " + slug + " is already in use")
	case dbkit.IsForeignKeyViolation(err):
		return ycerrors.Validation().Field("parent_id", "unknown", "Unknown parent category").Build()
	default:
		return fmt.Errorf("write category: %w", err)
	}
}
