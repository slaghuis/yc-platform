package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	"github.com/slaghuis/yc-platform/services/yc-product/internal/domain"
)

const bannerColumns = `b.id::text, b.category_id::text, c.name, b.text, b.background_image_id::text, b.large_image_id::text, b.sort_order`

func scanBanner(row pgx.Row) (domain.Banner, error) {
	var b domain.Banner
	err := row.Scan(&b.ID, &b.CategoryID, &b.CategoryName, &b.Text, &b.BackgroundImageID, &b.LargeImageID, &b.SortOrder)
	return b, err
}

// CreateBanner inserts a banner at the end of the shop's order.
func (Catalog) CreateBanner(ctx context.Context, tx pgx.Tx, tenantID string, in domain.BannerInput) (domain.Banner, error) {
	b, err := scanBanner(tx.QueryRow(ctx, `
		WITH inserted AS (
			INSERT INTO banners (tenant_id, category_id, text, background_image_id, large_image_id, sort_order)
			VALUES ($1, $2, $3, $4, $5, COALESCE((SELECT MAX(sort_order) + 1 FROM banners), 0))
			RETURNING id, category_id, text, background_image_id, large_image_id, sort_order
		)
		SELECT `+bannerColumns+`
		FROM inserted b JOIN categories c ON c.id = b.category_id`,
		tenantID, in.CategoryID, in.Text, in.BackgroundImageID, in.LargeImageID))
	if err != nil {
		return domain.Banner{}, bannerWriteError(err)
	}
	return b, nil
}

// GetBanner returns a banner in the transaction's shop.
func (Catalog) GetBanner(ctx context.Context, tx pgx.Tx, id string) (domain.Banner, error) {
	b, err := scanBanner(tx.QueryRow(ctx, `SELECT `+bannerColumns+`
		FROM banners b JOIN categories c ON c.id = b.category_id
		WHERE b.id = $1`, id))
	if dbkit.IsNoRows(err) {
		return domain.Banner{}, ycerrors.NotFound("Banner", id)
	}
	if err != nil {
		return domain.Banner{}, fmt.Errorf("get banner: %w", err)
	}
	return b, nil
}

// UpdateBanner replaces a banner's category, text and images. Order stays.
func (Catalog) UpdateBanner(ctx context.Context, tx pgx.Tx, id string, in domain.BannerInput) (domain.Banner, error) {
	b, err := scanBanner(tx.QueryRow(ctx, `
		WITH updated AS (
			UPDATE banners SET category_id = $2, text = $3, background_image_id = $4,
				large_image_id = $5, updated_at = NOW()
			WHERE id = $1
			RETURNING id, category_id, text, background_image_id, large_image_id, sort_order
		)
		SELECT `+bannerColumns+`
		FROM updated b JOIN categories c ON c.id = b.category_id`,
		id, in.CategoryID, in.Text, in.BackgroundImageID, in.LargeImageID))
	if dbkit.IsNoRows(err) {
		return domain.Banner{}, ycerrors.NotFound("Banner", id)
	}
	if err != nil {
		return domain.Banner{}, bannerWriteError(err)
	}
	return b, nil
}

// DeleteBanner deletes a banner.
func (Catalog) DeleteBanner(ctx context.Context, tx pgx.Tx, id string) error {
	tag, err := tx.Exec(ctx, `DELETE FROM banners WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete banner: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ycerrors.NotFound("Banner", id)
	}
	return nil
}

// Banners returns the shop's banners in display order. A banner whose
// category is gone is absent: the category foreign key removes it.
func (Catalog) Banners(ctx context.Context, tx pgx.Tx) ([]domain.Banner, error) {
	rows, err := tx.Query(ctx, `SELECT `+bannerColumns+`
		FROM banners b JOIN categories c ON c.id = b.category_id
		ORDER BY b.sort_order, b.created_at, b.id`)
	if err != nil {
		return nil, fmt.Errorf("banners: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Banner, error) { return scanBanner(r) })
	if err != nil {
		return nil, fmt.Errorf("banners: %w", err)
	}
	return out, nil
}

// BannerIDs returns the shop's banner ids in display order.
func (Catalog) BannerIDs(ctx context.Context, tx pgx.Tx) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT id::text FROM banners ORDER BY sort_order, created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("banner ids: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("banner ids: %w", err)
	}
	return out, nil
}

// BannerIDsByCategory returns banners that present categoryID.
func (Catalog) BannerIDsByCategory(ctx context.Context, tx pgx.Tx, categoryID string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT id::text FROM banners WHERE category_id = $1`, categoryID)
	if err != nil {
		return nil, fmt.Errorf("banners for category: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("banners for category: %w", err)
	}
	return out, nil
}

// ReorderBanners sets sort_order to each id's place in ids. The caller has
// checked that ids is exactly the shop's banners.
func (Catalog) ReorderBanners(ctx context.Context, tx pgx.Tx, ids []string) error {
	for i, id := range ids {
		tag, err := tx.Exec(ctx, `UPDATE banners SET sort_order = $2, updated_at = NOW() WHERE id = $1`, id, i)
		if err != nil {
			return fmt.Errorf("reorder banners: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ycerrors.NotFound("Banner", id)
		}
	}
	return nil
}

func bannerWriteError(err error) error {
	switch {
	case dbkit.IsUniqueViolation(err):
		return ycerrors.Conflict("This category already has a banner")
	case dbkit.IsForeignKeyViolation(err):
		return ycerrors.Validation().Field("category_id", "unknown", "Unknown category").Build()
	default:
		return fmt.Errorf("write banner: %w", err)
	}
}
