// Package repository is yc-review's Postgres access. Every method runs in
// the caller's transaction, so RLS (app.tenant_id) scopes the rows.
package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	"github.com/slaghuis/yc-platform/services/yc-review/internal/domain"
)

// Reviews reads and writes the review tables.
type Reviews struct{}

// Settings returns the shop's own settings, if it has set any.
func (Reviews) Settings(ctx context.Context, tx pgx.Tx, tenantID string) (domain.Settings, bool, error) {
	var s domain.Settings
	var mode string
	err := tx.QueryRow(ctx, `SELECT moderation_mode, require_purchase, allow_edit, updated_at
		FROM review_settings WHERE tenant_id = $1`, tenantID).
		Scan(&mode, &s.RequirePurchase, &s.AllowEdit, &s.UpdatedAt)
	if dbkit.IsNoRows(err) {
		return s, false, nil
	}
	if err != nil {
		return s, false, fmt.Errorf("get review settings: %w", err)
	}
	s.ModerationMode = domain.ModerationMode(mode)
	return s, true, nil
}

// PutSettings stores the shop's settings.
func (Reviews) PutSettings(ctx context.Context, tx pgx.Tx, tenantID string, s domain.Settings) error {
	_, err := tx.Exec(ctx, `INSERT INTO review_settings (tenant_id, moderation_mode, require_purchase, allow_edit, updated_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id) DO UPDATE SET moderation_mode = EXCLUDED.moderation_mode,
			require_purchase = EXCLUDED.require_purchase, allow_edit = EXCLUDED.allow_edit, updated_at = EXCLUDED.updated_at`,
		tenantID, string(s.ModerationMode), s.RequirePurchase, s.AllowEdit, s.UpdatedAt)
	if err != nil {
		return fmt.Errorf("put review settings: %w", err)
	}
	return nil
}

const reviewColumns = `id, product_id, user_id, rating, title, body, status, rejection_reason, verified_purchase,
	COALESCE(purchase_order_id::text, ''), purchase_check_until, next_check_at, shop_response, shop_response_at,
	approved_at, created_at, updated_at`

func scanReview(r pgx.Row) (domain.Review, error) {
	var v domain.Review
	var status string
	err := r.Scan(&v.ID, &v.ProductID, &v.UserID, &v.Rating, &v.Title, &v.Body, &status, &v.RejectionReason,
		&v.VerifiedPurchase, &v.PurchaseOrderID, &v.PurchaseCheckUntil, &v.NextCheckAt, &v.ShopResponse,
		&v.ShopResponseAt, &v.ApprovedAt, &v.CreatedAt, &v.UpdatedAt)
	v.Status = domain.Status(status)
	return v, err
}

// Review returns a review, locked for update when lock is set.
func (Reviews) Review(ctx context.Context, tx pgx.Tx, tenantID, id string, lock bool) (domain.Review, bool, error) {
	q := `SELECT ` + reviewColumns + ` FROM reviews WHERE tenant_id = $1 AND id = $2`
	if lock {
		q += ` FOR UPDATE`
	}
	v, err := scanReview(tx.QueryRow(ctx, q, tenantID, id))
	if dbkit.IsNoRows(err) {
		return v, false, nil
	}
	if err != nil {
		return v, false, fmt.Errorf("get review: %w", err)
	}
	return v, true, nil
}

// InsertReview records a new review. A second review of the product by the
// same user is a unique violation.
func (Reviews) InsertReview(ctx context.Context, tx pgx.Tx, tenantID string, v domain.Review) error {
	_, err := tx.Exec(ctx, `INSERT INTO reviews (id, tenant_id, product_id, user_id, rating, title, body, status,
			rejection_reason, verified_purchase, purchase_order_id, purchase_check_until, next_check_at,
			approved_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NULLIF($11, '')::uuid, $12, $13, $14, $15, $16)`,
		v.ID, tenantID, v.ProductID, v.UserID, v.Rating, v.Title, v.Body, string(v.Status), v.RejectionReason,
		v.VerifiedPurchase, v.PurchaseOrderID, v.PurchaseCheckUntil, v.NextCheckAt, v.ApprovedAt, v.CreatedAt, v.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert review: %w", err)
	}
	return nil
}

// UpdateReview stores a review's content, status, purchase check and
// response.
func (Reviews) UpdateReview(ctx context.Context, tx pgx.Tx, tenantID string, v domain.Review) error {
	_, err := tx.Exec(ctx, `UPDATE reviews SET rating = $3, title = $4, body = $5, status = $6, rejection_reason = $7,
			verified_purchase = $8, purchase_order_id = NULLIF($9, '')::uuid, purchase_check_until = $10,
			next_check_at = $11, shop_response = $12, shop_response_at = $13, approved_at = $14, updated_at = $15
		WHERE tenant_id = $1 AND id = $2`,
		tenantID, v.ID, v.Rating, v.Title, v.Body, string(v.Status), v.RejectionReason, v.VerifiedPurchase,
		v.PurchaseOrderID, v.PurchaseCheckUntil, v.NextCheckAt, v.ShopResponse, v.ShopResponseAt, v.ApprovedAt, v.UpdatedAt)
	if err != nil {
		return fmt.Errorf("update review: %w", err)
	}
	return nil
}

// DeleteReview deletes a review.
func (Reviews) DeleteReview(ctx context.Context, tx pgx.Tx, tenantID, id string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM reviews WHERE tenant_id = $1 AND id = $2`, tenantID, id); err != nil {
		return fmt.Errorf("delete review: %w", err)
	}
	return nil
}

// Cursor is the position after the last row of a page.
type Cursor struct {
	CreatedAt time.Time `json:"c"`
	ID        string    `json:"i"`
}

// Filter selects reviews. Empty fields match any.
type Filter struct {
	ProductID string
	UserID    string
	Status    domain.Status
}

// ListReviews returns up to limit reviews matching f, newest first, after
// the cursor.
func (Reviews) ListReviews(ctx context.Context, tx pgx.Tx, tenantID string, f Filter, after *Cursor, limit int) ([]domain.Review, error) {
	q := `SELECT ` + reviewColumns + ` FROM reviews WHERE tenant_id = $1`
	args := []any{tenantID}
	if f.ProductID != "" {
		args = append(args, f.ProductID)
		q += fmt.Sprintf(` AND product_id = $%d`, len(args))
	}
	if f.UserID != "" {
		args = append(args, f.UserID)
		q += fmt.Sprintf(` AND user_id = $%d`, len(args))
	}
	if f.Status != "" {
		args = append(args, string(f.Status))
		q += fmt.Sprintf(` AND status = $%d`, len(args))
	}
	if after != nil {
		args = append(args, after.CreatedAt, after.ID)
		q += fmt.Sprintf(` AND (created_at, id) < ($%d, $%d)`, len(args)-1, len(args))
	}
	args = append(args, limit)
	q += fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d`, len(args))
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list reviews: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Review, error) { return scanReview(r) })
	if err != nil {
		return nil, fmt.Errorf("list reviews: %w", err)
	}
	return out, nil
}

// Distribution counts a product's approved reviews by rating; index 0 is
// one star.
func (Reviews) Distribution(ctx context.Context, tx pgx.Tx, tenantID, productID string) ([domain.MaxRating]int64, error) {
	var out [domain.MaxRating]int64
	rows, err := tx.Query(ctx, `SELECT rating, COUNT(*) FROM reviews
		WHERE tenant_id = $1 AND product_id = $2 AND status = 'approved' GROUP BY rating`, tenantID, productID)
	if err != nil {
		return out, fmt.Errorf("rating distribution: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var rating int
		var n int64
		if err := rows.Scan(&rating, &n); err != nil {
			return out, fmt.Errorf("rating distribution: %w", err)
		}
		if rating >= domain.MinRating && rating <= domain.MaxRating {
			out[rating-1] = n
		}
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("rating distribution: %w", err)
	}
	return out, nil
}

// Stats returns a product's stats; zero when it has none.
func (Reviews) Stats(ctx context.Context, tx pgx.Tx, tenantID, productID string) (domain.Stats, error) {
	s := domain.Stats{ProductID: productID}
	err := tx.QueryRow(ctx, `SELECT rating_count, rating_sum, like_count, version FROM product_stats
		WHERE tenant_id = $1 AND product_id = $2`, tenantID, productID).
		Scan(&s.RatingCount, &s.RatingSum, &s.LikeCount, &s.Version)
	if err != nil && !dbkit.IsNoRows(err) {
		return s, fmt.Errorf("get product stats: %w", err)
	}
	return s, nil
}

// RefreshStats recounts a product's approved reviews and likes and bumps
// its version. The stats row is locked first, so concurrent changes to the
// product count in turn and versions follow commit order.
func (Reviews) RefreshStats(ctx context.Context, tx pgx.Tx, tenantID, productID string) (domain.Stats, error) {
	_, err := tx.Exec(ctx, `INSERT INTO product_stats (tenant_id, product_id) VALUES ($1, $2)
		ON CONFLICT (tenant_id, product_id) DO NOTHING`, tenantID, productID)
	if err != nil {
		return domain.Stats{}, fmt.Errorf("create product stats: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM product_stats WHERE tenant_id = $1 AND product_id = $2 FOR UPDATE`,
		tenantID, productID); err != nil {
		return domain.Stats{}, fmt.Errorf("lock product stats: %w", err)
	}
	s := domain.Stats{ProductID: productID}
	err = tx.QueryRow(ctx, `UPDATE product_stats SET
			rating_count = r.n, rating_sum = r.total, like_count = l.n, version = version + 1
		FROM (SELECT COUNT(*) AS n, COALESCE(SUM(rating), 0) AS total FROM reviews
				WHERE tenant_id = $1 AND product_id = $2 AND status = 'approved') r,
			(SELECT COUNT(*) AS n FROM product_likes WHERE tenant_id = $1 AND product_id = $2) l
		WHERE tenant_id = $1 AND product_id = $2
		RETURNING rating_count, rating_sum, like_count, version`, tenantID, productID).
		Scan(&s.RatingCount, &s.RatingSum, &s.LikeCount, &s.Version)
	if err != nil {
		return s, fmt.Errorf("refresh product stats: %w", err)
	}
	return s, nil
}

// AddLike records that the user likes the product and reports whether it
// is new.
func (Reviews) AddLike(ctx context.Context, tx pgx.Tx, tenantID, productID, userID string, now time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, `INSERT INTO product_likes (tenant_id, product_id, user_id, created_at)
		VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, tenantID, productID, userID, now)
	if err != nil {
		return false, fmt.Errorf("add like: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// RemoveLike deletes the user's like and reports whether there was one.
func (Reviews) RemoveLike(ctx context.Context, tx pgx.Tx, tenantID, productID, userID string) (bool, error) {
	tag, err := tx.Exec(ctx, `DELETE FROM product_likes WHERE tenant_id = $1 AND product_id = $2 AND user_id = $3`,
		tenantID, productID, userID)
	if err != nil {
		return false, fmt.Errorf("remove like: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Forgotten is what ForgetUser changed.
type Forgotten struct {
	Reviews int64
	Prompts int64
	// LikedProducts are the products whose likes were removed.
	LikedProducts []string
}

// ForgetUser deletes userID's likes and review prompts and gives its
// reviews to pseudonym (ADR-005).
func (Reviews) ForgetUser(ctx context.Context, tx pgx.Tx, tenantID, userID, pseudonym string) (Forgotten, error) {
	var f Forgotten
	rows, err := tx.Query(ctx, `DELETE FROM product_likes WHERE tenant_id = $1 AND user_id = $2 RETURNING product_id::text`,
		tenantID, userID)
	if err != nil {
		return f, fmt.Errorf("delete likes: %w", err)
	}
	if f.LikedProducts, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return f, fmt.Errorf("delete likes: %w", err)
	}
	tag, err := tx.Exec(ctx, `UPDATE reviews SET user_id = $3 WHERE tenant_id = $1 AND user_id = $2`,
		tenantID, userID, pseudonym)
	if err != nil {
		return f, fmt.Errorf("pseudonymise reviews: %w", err)
	}
	f.Reviews = tag.RowsAffected()
	if tag, err = tx.Exec(ctx, `DELETE FROM review_prompts WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID); err != nil {
		return f, fmt.Errorf("delete prompts: %w", err)
	}
	f.Prompts = tag.RowsAffected()
	return f, nil
}

// Like is a product a user likes.
type Like struct {
	ProductID string
	LikedAt   time.Time
}

// Likes returns up to limit of the user's likes, newest first, after the
// cursor (ID is the product ID).
func (Reviews) Likes(ctx context.Context, tx pgx.Tx, tenantID, userID string, after *Cursor, limit int) ([]Like, error) {
	q := `SELECT product_id, created_at FROM product_likes WHERE tenant_id = $1 AND user_id = $2`
	args := []any{tenantID, userID}
	if after != nil {
		args = append(args, after.CreatedAt, after.ID)
		q += ` AND (created_at, product_id) < ($3, $4)`
	}
	args = append(args, limit)
	q += fmt.Sprintf(` ORDER BY created_at DESC, product_id DESC LIMIT $%d`, len(args))
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list likes: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Like, error) {
		var l Like
		err := r.Scan(&l.ProductID, &l.LikedAt)
		return l, err
	})
	if err != nil {
		return nil, fmt.Errorf("list likes: %w", err)
	}
	return out, nil
}

// Due is a row a background job picks up, in its shop.
type Due struct {
	TenantID string
	ID       string
}

// DueChecks returns reviews whose purchase check is due, across shops.
// Call it in a platform transaction.
func (Reviews) DueChecks(ctx context.Context, tx pgx.Tx, now time.Time, limit int) ([]Due, error) {
	return due(ctx, tx, `SELECT tenant_id, id FROM reviews
		WHERE purchase_check_until IS NOT NULL AND next_check_at <= $1 ORDER BY next_check_at LIMIT $2`, now, limit)
}

// LockDueCheck locks a review whose purchase check is still due.
func (Reviews) LockDueCheck(ctx context.Context, tx pgx.Tx, tenantID, id string, now time.Time) (domain.Review, bool, error) {
	v, err := scanReview(tx.QueryRow(ctx, `SELECT `+reviewColumns+` FROM reviews
		WHERE tenant_id = $1 AND id = $2 AND purchase_check_until IS NOT NULL AND next_check_at <= $3
		FOR UPDATE SKIP LOCKED`, tenantID, id, now))
	if dbkit.IsNoRows(err) {
		return v, false, nil
	}
	if err != nil {
		return v, false, fmt.Errorf("lock purchase check: %w", err)
	}
	return v, true, nil
}

// InsertPrompt schedules a delivered order's review prompt, once.
func (Reviews) InsertPrompt(ctx context.Context, tx pgx.Tx, tenantID, orderID, userID string, dueAt time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO review_prompts (tenant_id, order_id, user_id, due_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, order_id) DO NOTHING`, tenantID, orderID, userID, dueAt)
	if err != nil {
		return fmt.Errorf("insert review prompt: %w", err)
	}
	return nil
}

// DuePrompts returns review prompts that are due, across shops. Call it in
// a platform transaction.
func (Reviews) DuePrompts(ctx context.Context, tx pgx.Tx, now time.Time, limit int) ([]Due, error) {
	return due(ctx, tx, `SELECT tenant_id, order_id FROM review_prompts
		WHERE done_at IS NULL AND due_at <= $1 ORDER BY due_at LIMIT $2`, now, limit)
}

// Prompt is a scheduled review prompt.
type Prompt struct {
	OrderID string
	UserID  string
}

// LockDuePrompt locks a prompt that is still due.
func (Reviews) LockDuePrompt(ctx context.Context, tx pgx.Tx, tenantID, orderID string, now time.Time) (Prompt, bool, error) {
	var p Prompt
	err := tx.QueryRow(ctx, `SELECT order_id, user_id FROM review_prompts
		WHERE tenant_id = $1 AND order_id = $2 AND done_at IS NULL AND due_at <= $3 FOR UPDATE SKIP LOCKED`,
		tenantID, orderID, now).Scan(&p.OrderID, &p.UserID)
	if dbkit.IsNoRows(err) {
		return p, false, nil
	}
	if err != nil {
		return p, false, fmt.Errorf("lock review prompt: %w", err)
	}
	return p, true, nil
}

// FinishPrompt marks a prompt done.
func (Reviews) FinishPrompt(ctx context.Context, tx pgx.Tx, tenantID, orderID string, now time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE review_prompts SET done_at = $3 WHERE tenant_id = $1 AND order_id = $2`,
		tenantID, orderID, now)
	if err != nil {
		return fmt.Errorf("finish review prompt: %w", err)
	}
	return nil
}

// DelayPrompt moves a prompt's due time.
func (Reviews) DelayPrompt(ctx context.Context, tx pgx.Tx, tenantID, orderID string, dueAt time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE review_prompts SET due_at = $3 WHERE tenant_id = $1 AND order_id = $2`,
		tenantID, orderID, dueAt)
	if err != nil {
		return fmt.Errorf("delay review prompt: %w", err)
	}
	return nil
}

// Reviewed returns which of productIDs the user has reviewed.
func (Reviews) Reviewed(ctx context.Context, tx pgx.Tx, tenantID, userID string, productIDs []string) (map[string]bool, error) {
	rows, err := tx.Query(ctx, `SELECT product_id::text FROM reviews
		WHERE tenant_id = $1 AND user_id = $2 AND product_id::text = ANY($3)`, tenantID, userID, productIDs)
	if err != nil {
		return nil, fmt.Errorf("reviewed products: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("reviewed products: %w", err)
	}
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

func due(ctx context.Context, tx pgx.Tx, q string, now time.Time, limit int) ([]Due, error) {
	rows, err := tx.Query(ctx, q, now, limit)
	if err != nil {
		return nil, fmt.Errorf("due rows: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Due, error) {
		var d Due
		err := r.Scan(&d.TenantID, &d.ID)
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("due rows: %w", err)
	}
	return out, nil
}
