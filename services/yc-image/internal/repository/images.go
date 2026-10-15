// Package repository is yc-image's Postgres access. Every method runs in
// the caller's transaction, so RLS (app.tenant_id) scopes it.
package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	"github.com/slaghuis/yc-platform/services/yc-image/internal/domain"
)

// Images reads and writes images and their variants.
type Images struct{}

const imageColumns = `id, tenant_id, uploader_id, status, content_type, COALESCE(width, 0), COALESCE(height, 0),
	COALESCE(bytes, 0), COALESCE(checksum_sha256, ''), COALESCE(owner_ref, ''), COALESCE(failure_reason, ''),
	created_at, processed_at`

// Create inserts an image.
func (Images) Create(ctx context.Context, tx pgx.Tx, img domain.Image) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO images (id, tenant_id, uploader_id, status, content_type, bytes, checksum_sha256, owner_ref, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), NULLIF($8, ''), $9)`,
		img.ID, img.TenantID, img.UploaderID, img.Status, img.ContentType, img.Bytes, img.Checksum, img.OwnerRef, img.CreatedAt)
	if err != nil {
		return fmt.Errorf("create image: %w", err)
	}
	return nil
}

// Get returns an image with its variants, locked when forUpdate. Deleted
// images are not found.
func (Images) Get(ctx context.Context, tx pgx.Tx, id string, forUpdate bool) (domain.Image, error) {
	q := `SELECT ` + imageColumns + ` FROM images WHERE id = $1 AND status <> 'deleted'`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	var img domain.Image
	err := tx.QueryRow(ctx, q, id).Scan(&img.ID, &img.TenantID, &img.UploaderID, &img.Status, &img.ContentType,
		&img.Width, &img.Height, &img.Bytes, &img.Checksum, &img.OwnerRef, &img.FailureReason, &img.CreatedAt, &img.ProcessedAt)
	if dbkit.IsNoRows(err) {
		return domain.Image{}, ycerrors.NotFound("Image", id)
	}
	if err != nil {
		return domain.Image{}, fmt.Errorf("get image: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT variant, format, width, height, bytes, storage_key, url FROM image_variants
		WHERE image_id = $1 ORDER BY width DESC, variant, format DESC`, id)
	if err != nil {
		return domain.Image{}, fmt.Errorf("image variants: %w", err)
	}
	img.Variants, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Variant, error) {
		var v domain.Variant
		err := r.Scan(&v.Variant, &v.Format, &v.Width, &v.Height, &v.Bytes, &v.Key, &v.URL)
		return v, err
	})
	if err != nil {
		return domain.Image{}, fmt.Errorf("image variants: %w", err)
	}
	return img, nil
}

// MarkProcessing records a finalised upload.
func (Images) MarkProcessing(ctx context.Context, tx pgx.Tx, id, contentType string, size int64, checksum string) error {
	_, err := tx.Exec(ctx, `UPDATE images SET status = 'processing', content_type = $2, bytes = $3, checksum_sha256 = $4
		WHERE id = $1`, id, contentType, size, checksum)
	if err != nil {
		return fmt.Errorf("mark processing: %w", err)
	}
	return nil
}

// MarkReady stores the variants and makes the image servable.
func (Images) MarkReady(ctx context.Context, tx pgx.Tx, img domain.Image, width, height int, variants []domain.Variant, at time.Time) error {
	if _, err := tx.Exec(ctx, `UPDATE images SET status = 'ready', width = $2, height = $3, processed_at = $4 WHERE id = $1`,
		img.ID, width, height, at); err != nil {
		return fmt.Errorf("mark ready: %w", err)
	}
	for _, v := range variants {
		if _, err := tx.Exec(ctx, `
			INSERT INTO image_variants (tenant_id, image_id, variant, format, width, height, bytes, storage_key, url)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (image_id, variant, format) DO UPDATE SET width = EXCLUDED.width, height = EXCLUDED.height,
				bytes = EXCLUDED.bytes, storage_key = EXCLUDED.storage_key, url = EXCLUDED.url`,
			img.TenantID, img.ID, v.Variant, v.Format, v.Width, v.Height, v.Bytes, v.Key, v.URL); err != nil {
			return fmt.Errorf("insert variant: %w", err)
		}
	}
	return nil
}

// MarkFailed records why an image cannot be served.
func (Images) MarkFailed(ctx context.Context, tx pgx.Tx, id, reason string) error {
	if _, err := tx.Exec(ctx, `UPDATE images SET status = 'failed', failure_reason = $2 WHERE id = $1`, id, reason); err != nil {
		return fmt.Errorf("mark failed: %w", err)
	}
	return nil
}

// MarkDeleted deletes an image's variants and tombstones the image.
func (Images) MarkDeleted(ctx context.Context, tx pgx.Tx, id string, at time.Time) error {
	if _, err := tx.Exec(ctx, `DELETE FROM image_variants WHERE image_id = $1`, id); err != nil {
		return fmt.Errorf("delete variants: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE images SET status = 'deleted', deleted_at = $2 WHERE id = $1`, id, at); err != nil {
		return fmt.Errorf("mark deleted: %w", err)
	}
	return nil
}
