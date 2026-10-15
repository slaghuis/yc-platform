// Package repository is yc-cart's Postgres access. Every method runs in
// the caller's transaction, so RLS (app.tenant_id) scopes the rows.
package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	money "github.com/slaghuis/yc-platform/libs/yc-money"
	"github.com/slaghuis/yc-platform/services/yc-cart/internal/domain"
)

// Carts reads and writes carts and their lines.
type Carts struct{}

// Get returns userID's cart of cartType with its lines, locked when
// forUpdate, or false when there is none.
func (Carts) Get(ctx context.Context, tx pgx.Tx, tenantID, userID, cartType string, forUpdate bool) (domain.Cart, bool, error) {
	q := `SELECT id, user_id, cart_type, created_at, updated_at FROM carts
		WHERE tenant_id = $1 AND user_id = $2 AND cart_type = $3`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	var c domain.Cart
	err := tx.QueryRow(ctx, q, tenantID, userID, cartType).Scan(&c.ID, &c.UserID, &c.Type, &c.CreatedAt, &c.UpdatedAt)
	if dbkit.IsNoRows(err) {
		return domain.Cart{}, false, nil
	}
	if err != nil {
		return domain.Cart{}, false, fmt.Errorf("get cart: %w", err)
	}
	if c.Items, err = items(ctx, tx, c.ID); err != nil {
		return domain.Cart{}, false, err
	}
	return c, true, nil
}

// List returns userID's carts with their lines.
func (Carts) List(ctx context.Context, tx pgx.Tx, tenantID, userID string) ([]domain.Cart, error) {
	rows, err := tx.Query(ctx, `SELECT id, user_id, cart_type, created_at, updated_at FROM carts
		WHERE tenant_id = $1 AND user_id = $2 ORDER BY cart_type`, tenantID, userID)
	if err != nil {
		return nil, fmt.Errorf("list carts: %w", err)
	}
	carts, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Cart, error) {
		var c domain.Cart
		err := r.Scan(&c.ID, &c.UserID, &c.Type, &c.CreatedAt, &c.UpdatedAt)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("list carts: %w", err)
	}
	for i := range carts {
		if carts[i].Items, err = items(ctx, tx, carts[i].ID); err != nil {
			return nil, err
		}
	}
	return carts, nil
}

// Create creates userID's cart of cartType unless it exists, reporting
// whether it did. Lock it with Get afterwards.
func (Carts) Create(ctx context.Context, tx pgx.Tx, tenantID, userID, cartType string, customer bool, now time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, `INSERT INTO carts (tenant_id, user_id, cart_type, user_type, created_at, updated_at)
		VALUES ($1, $2, $3, CASE WHEN $4 THEN 'customer' ELSE 'visitor' END, $5, $5)
		ON CONFLICT (tenant_id, user_id, cart_type) DO NOTHING`,
		tenantID, userID, cartType, customer, now)
	if err != nil {
		return false, fmt.Errorf("create cart: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Touch records activity on a cart, which ends an abandoned spell. A
// customer's activity makes it a customer's cart.
func (Carts) Touch(ctx context.Context, tx pgx.Tx, cartID string, customer bool, now time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE carts SET updated_at = $2, abandoned_at = NULL,
			user_type = CASE WHEN $3 THEN 'customer' ELSE user_type END
		WHERE id = $1`, cartID, now, customer)
	if err != nil {
		return fmt.Errorf("touch cart: %w", err)
	}
	return nil
}

// Due is a cart a background job picks up, in its shop.
type Due struct {
	TenantID string
	CartID   string
}

// IdleCarts returns customers' carts of cartTypes with lines and no
// activity since before that have not been announced as abandoned, across
// shops. Call it in a platform transaction.
func (Carts) IdleCarts(ctx context.Context, tx pgx.Tx, cartTypes []string, before time.Time, limit int) ([]Due, error) {
	rows, err := tx.Query(ctx, `SELECT c.tenant_id, c.id FROM carts c
		WHERE c.user_type = 'customer' AND c.abandoned_at IS NULL AND c.updated_at <= $2
			AND c.cart_type = ANY($1) AND EXISTS (SELECT 1 FROM cart_items i WHERE i.cart_id = c.id)
		ORDER BY c.updated_at LIMIT $3`, cartTypes, before, limit)
	if err != nil {
		return nil, fmt.Errorf("idle carts: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Due, error) {
		var d Due
		err := r.Scan(&d.TenantID, &d.CartID)
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("idle carts: %w", err)
	}
	return out, nil
}

// LockIdle locks a cart that is still idle since before and not announced,
// with its lines.
func (Carts) LockIdle(ctx context.Context, tx pgx.Tx, tenantID, cartID string, before time.Time) (domain.Cart, bool, error) {
	var c domain.Cart
	err := tx.QueryRow(ctx, `SELECT id, user_id, cart_type, created_at, updated_at FROM carts
		WHERE tenant_id = $1 AND id = $2 AND user_type = 'customer' AND abandoned_at IS NULL AND updated_at <= $3
		FOR UPDATE SKIP LOCKED`, tenantID, cartID, before).Scan(&c.ID, &c.UserID, &c.Type, &c.CreatedAt, &c.UpdatedAt)
	if dbkit.IsNoRows(err) {
		return c, false, nil
	}
	if err != nil {
		return c, false, fmt.Errorf("lock idle cart: %w", err)
	}
	if c.Items, err = items(ctx, tx, c.ID); err != nil {
		return c, false, err
	}
	return c, true, nil
}

// MarkAbandoned records that the cart was announced as abandoned.
func (Carts) MarkAbandoned(ctx context.Context, tx pgx.Tx, cartID string, now time.Time) error {
	if _, err := tx.Exec(ctx, `UPDATE carts SET abandoned_at = $2 WHERE id = $1`, cartID, now); err != nil {
		return fmt.Errorf("mark cart abandoned: %w", err)
	}
	return nil
}

// Abandoned is a cart announced as abandoned and not used since.
type Abandoned struct {
	CartID         string
	UserID         string
	CartType       string
	Lines          int
	Quantity       int
	LastActivityAt time.Time
	AbandonedAt    time.Time
}

// AbandonedCursor is the position after the last cart of a page.
type AbandonedCursor struct {
	AbandonedAt time.Time `json:"a"`
	ID          string    `json:"i"`
}

// AbandonedCarts returns up to limit abandoned carts, most recently
// abandoned first, after the cursor.
func (Carts) AbandonedCarts(ctx context.Context, tx pgx.Tx, tenantID string, after *AbandonedCursor, limit int) ([]Abandoned, error) {
	q := `SELECT c.id, c.user_id, c.cart_type, COUNT(i.sku_id), COALESCE(SUM(i.quantity), 0), c.updated_at, c.abandoned_at
		FROM carts c LEFT JOIN cart_items i ON i.cart_id = c.id
		WHERE c.tenant_id = $1 AND c.abandoned_at IS NOT NULL`
	args := []any{tenantID}
	if after != nil {
		args = append(args, after.AbandonedAt, after.ID)
		q += ` AND (c.abandoned_at, c.id) < ($2, $3)`
	}
	args = append(args, limit)
	q += fmt.Sprintf(` GROUP BY c.id ORDER BY c.abandoned_at DESC, c.id DESC LIMIT $%d`, len(args))
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list abandoned carts: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Abandoned, error) {
		var a Abandoned
		err := r.Scan(&a.CartID, &a.UserID, &a.CartType, &a.Lines, &a.Quantity, &a.LastActivityAt, &a.AbandonedAt)
		return a, err
	})
	if err != nil {
		return nil, fmt.Errorf("list abandoned carts: %w", err)
	}
	return out, nil
}

// Delete removes a cart and its lines.
func (Carts) Delete(ctx context.Context, tx pgx.Tx, cartID string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM carts WHERE id = $1`, cartID); err != nil {
		return fmt.Errorf("delete cart: %w", err)
	}
	return nil
}

// PutItem inserts a line or replaces its quantity; an existing line keeps
// its price snapshot and added_at.
func (Carts) PutItem(ctx context.Context, tx pgx.Tx, tenantID, cartID string, it domain.Item, now time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO cart_items (tenant_id, cart_id, sku_id, quantity, unit_price_minor, unit_price_currency, added_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (cart_id, sku_id) DO UPDATE SET quantity = EXCLUDED.quantity, updated_at = EXCLUDED.updated_at`,
		tenantID, cartID, it.SKUID, it.Quantity, it.UnitPrice.AmountMinor, it.UnitPrice.Currency, it.AddedAt, now)
	if err != nil {
		return fmt.Errorf("put cart item: %w", err)
	}
	return nil
}

// DeleteItem removes a line.
func (Carts) DeleteItem(ctx context.Context, tx pgx.Tx, cartID, skuID string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM cart_items WHERE cart_id = $1 AND sku_id = $2`, cartID, skuID); err != nil {
		return fmt.Errorf("delete cart item: %w", err)
	}
	return nil
}

// RecordConversion marks a checkout session as removed from its cart;
// false means it already was.
func (Carts) RecordConversion(ctx context.Context, tx pgx.Tx, tenantID, sessionID, orderID string, now time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO cart_conversions (tenant_id, session_id, order_id, converted_at)
		VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, tenantID, sessionID, orderID, now)
	if err != nil {
		return false, fmt.Errorf("record cart conversion: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Clear removes every line of a cart.
func (Carts) Clear(ctx context.Context, tx pgx.Tx, cartID string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM cart_items WHERE cart_id = $1`, cartID); err != nil {
		return fmt.Errorf("clear cart: %w", err)
	}
	return nil
}

// SKULine is a cart line located by SKU.
type SKULine struct {
	Cart domain.Cart // without Items
	Item domain.Item
}

// LinesWithSKUs returns every line holding one of skuIDs, with its cart
// locked.
func (Carts) LinesWithSKUs(ctx context.Context, tx pgx.Tx, tenantID string, skuIDs []string) ([]SKULine, error) {
	rows, err := tx.Query(ctx, `
		SELECT c.id, c.user_id, c.cart_type, i.sku_id, i.quantity, i.unit_price_minor, i.unit_price_currency,
			i.added_at, i.updated_at
		FROM cart_items i JOIN carts c ON c.id = i.cart_id
		WHERE i.tenant_id = $1 AND i.sku_id = ANY($2::uuid[])
		ORDER BY c.id, i.sku_id
		FOR UPDATE OF c`, tenantID, skuIDs)
	if err != nil {
		return nil, fmt.Errorf("lines with skus: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (SKULine, error) {
		var (
			l        SKULine
			minor    int64
			currency string
		)
		err := r.Scan(&l.Cart.ID, &l.Cart.UserID, &l.Cart.Type, &l.Item.SKUID, &l.Item.Quantity, &minor, &currency,
			&l.Item.AddedAt, &l.Item.UpdatedAt)
		l.Item.UnitPrice = money.New(minor, currency)
		return l, err
	})
	if err != nil {
		return nil, fmt.Errorf("lines with skus: %w", err)
	}
	return out, nil
}

// DeleteUser removes a user's carts, their lines and display currency,
// returning how many carts and currencies went.
func (Carts) DeleteUser(ctx context.Context, tx pgx.Tx, tenantID, userID string) (int64, error) {
	carts, err := tx.Exec(ctx, `DELETE FROM carts WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID)
	if err != nil {
		return 0, fmt.Errorf("delete carts: %w", err)
	}
	currencies, err := tx.Exec(ctx, `DELETE FROM display_currencies WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID)
	if err != nil {
		return 0, fmt.Errorf("delete display currency: %w", err)
	}
	return carts.RowsAffected() + currencies.RowsAffected(), nil
}

// DisplayCurrency returns userID's display currency, or "" when unknown.
func (Carts) DisplayCurrency(ctx context.Context, tx pgx.Tx, tenantID, userID string) (string, error) {
	var c string
	err := tx.QueryRow(ctx, `SELECT display_currency FROM display_currencies WHERE tenant_id = $1 AND user_id = $2`,
		tenantID, userID).Scan(&c)
	if dbkit.IsNoRows(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get display currency: %w", err)
	}
	return c, nil
}

// SetDisplayCurrency records userID's display currency unless a later
// change is already recorded.
func (Carts) SetDisplayCurrency(ctx context.Context, tx pgx.Tx, tenantID, userID, currency string, at time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO display_currencies (tenant_id, user_id, display_currency, changed_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, user_id) DO UPDATE SET display_currency = EXCLUDED.display_currency,
			changed_at = EXCLUDED.changed_at
		WHERE display_currencies.changed_at <= EXCLUDED.changed_at`, tenantID, userID, currency, at)
	if err != nil {
		return fmt.Errorf("set display currency: %w", err)
	}
	return nil
}

func items(ctx context.Context, tx pgx.Tx, cartID string) ([]domain.Item, error) {
	rows, err := tx.Query(ctx, `SELECT sku_id, quantity, unit_price_minor, unit_price_currency, added_at, updated_at
		FROM cart_items WHERE cart_id = $1 ORDER BY added_at, sku_id`, cartID)
	if err != nil {
		return nil, fmt.Errorf("list cart items: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Item, error) {
		var (
			it       domain.Item
			minor    int64
			currency string
		)
		err := r.Scan(&it.SKUID, &it.Quantity, &minor, &currency, &it.AddedAt, &it.UpdatedAt)
		it.UnitPrice = money.New(minor, currency)
		return it, err
	})
	if err != nil {
		return nil, fmt.Errorf("list cart items: %w", err)
	}
	return out, nil
}
