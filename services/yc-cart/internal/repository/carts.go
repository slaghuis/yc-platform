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
func (Carts) Create(ctx context.Context, tx pgx.Tx, tenantID, userID, cartType string, now time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, `INSERT INTO carts (tenant_id, user_id, cart_type, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $4) ON CONFLICT (tenant_id, user_id, cart_type) DO NOTHING`,
		tenantID, userID, cartType, now)
	if err != nil {
		return false, fmt.Errorf("create cart: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Touch records activity on a cart.
func (Carts) Touch(ctx context.Context, tx pgx.Tx, cartID string, now time.Time) error {
	if _, err := tx.Exec(ctx, `UPDATE carts SET updated_at = $2 WHERE id = $1`, cartID, now); err != nil {
		return fmt.Errorf("touch cart: %w", err)
	}
	return nil
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
