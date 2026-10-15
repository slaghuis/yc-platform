// Package repository stores yc-analytics' facts, aggregates and milestones.
package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/slaghuis/yc-platform/services/yc-analytics/internal/domain"
)

// Facts stores the facts and the shop dimension.
type Facts struct{}

// PutTenant upserts a shop.
func (Facts) PutTenant(ctx context.Context, tx pgx.Tx, t domain.Tenant, now time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO dim_tenant (tenant_id, name, store_currency, provisioned_at, updated_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id) DO UPDATE SET name = EXCLUDED.name,
			store_currency = EXCLUDED.store_currency, updated_at = EXCLUDED.updated_at`,
		t.TenantID, t.Name, t.StoreCurrency, t.ProvisionedAt, now)
	if err != nil {
		return fmt.Errorf("put tenant: %w", err)
	}
	return nil
}

// StoreCurrency returns a shop's store currency, if it is known.
func (Facts) StoreCurrency(ctx context.Context, tx pgx.Tx, tenantID string) (string, bool, error) {
	var c string
	err := tx.QueryRow(ctx, `SELECT store_currency FROM dim_tenant WHERE tenant_id = $1`, tenantID).Scan(&c)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store currency: %w", err)
	}
	return c, true, nil
}

// InsertOrder stores an order and its lines and reports whether it is new.
func (Facts) InsertOrder(ctx context.Context, tx pgx.Tx, o domain.Order) (bool, error) {
	var txID *string
	if o.TransactionID != "" {
		txID = &o.TransactionID
	}
	var items int64
	for _, it := range o.Items {
		items += it.Quantity
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO fact_order (order_id, tenant_id, date_key, order_number, user_id, user_type, status,
			item_count, currency, items_minor, tax_minor, shipping_minor, total_minor, transaction_id, placed_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'placed', $7, $8, $9, $10, $11, $12, $13, $14)
		ON CONFLICT (order_id) DO NOTHING`,
		o.OrderID, o.TenantID, domain.Date(o.PlacedAt), o.OrderNumber, o.UserID, o.UserType,
		items, o.Currency, o.ItemsMinor, o.TaxMinor, o.ShippingMinor, o.TotalMinor, txID, o.PlacedAt)
	if err != nil {
		return false, fmt.Errorf("insert order fact: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	batch := &pgx.Batch{}
	for i, it := range o.Items {
		batch.Queue(`
			INSERT INTO fact_order_item (order_id, line, tenant_id, date_key, sku_id, sku_code, name, quantity,
				currency, unit_price_minor, line_total_minor)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			o.OrderID, i+1, o.TenantID, domain.Date(o.PlacedAt), it.SKUID, it.SKUCode, it.Name, it.Quantity,
			o.Currency, it.UnitPriceMinor, it.LineTotalMinor)
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return false, fmt.Errorf("insert order lines: %w", err)
	}
	return true, nil
}

// CancelOrder marks an order cancelled and returns its date; ok is false
// when the order is not recorded (yet).
func (Facts) CancelOrder(ctx context.Context, tx pgx.Tx, orderID string, at time.Time) (time.Time, bool, error) {
	return orderDate(tx.QueryRow(ctx, `
		UPDATE fact_order SET status = 'cancelled', cancelled_at = COALESCE(cancelled_at, $2)
		WHERE order_id = $1 RETURNING date_key`, orderID, at))
}

// RefundOrder sets an order's total refunded and returns its date.
func (Facts) RefundOrder(ctx context.Context, tx pgx.Tx, orderID string, totalRefunded int64) (time.Time, bool, error) {
	return orderDate(tx.QueryRow(ctx, `
		UPDATE fact_order SET refunded_minor = GREATEST(refunded_minor, $2)
		WHERE order_id = $1 RETURNING date_key`, orderID, totalRefunded))
}

// DeliverOrder records when an order was delivered.
func (Facts) DeliverOrder(ctx context.Context, tx pgx.Tx, orderID string, at time.Time) (bool, error) {
	_, ok, err := orderDate(tx.QueryRow(ctx, `
		UPDATE fact_order SET delivered_at = COALESCE(delivered_at, $2)
		WHERE order_id = $1 RETURNING date_key`, orderID, at))
	return ok, err
}

func orderDate(row pgx.Row) (time.Time, bool, error) {
	var d time.Time
	err := row.Scan(&d)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, false, nil
	}
	if err != nil {
		return d, false, fmt.Errorf("update order fact: %w", err)
	}
	return d, true, nil
}

// OrderTotals counts a shop's orders and sums their revenue, net of
// refunds, in currency.
func (Facts) OrderTotals(ctx context.Context, tx pgx.Tx, tenantID, currency string) (orders, revenue int64, err error) {
	err = tx.QueryRow(ctx, `
		SELECT COUNT(*),
			COALESCE(SUM(total_minor - refunded_minor) FILTER (WHERE status = 'placed' AND currency = $2), 0)::bigint
		FROM fact_order WHERE tenant_id = $1`, tenantID, currency).Scan(&orders, &revenue)
	if err != nil {
		return 0, 0, fmt.Errorf("order totals: %w", err)
	}
	return orders, revenue, nil
}

// PutPayment records a payment event on its transaction and returns the
// transaction's date.
func (Facts) PutPayment(ctx context.Context, tx pgx.Tx, p domain.Payment) (time.Time, error) {
	var authorized, succeeded, failed *time.Time
	var reason *string
	switch p.Outcome {
	case domain.PaymentAuthorized:
		authorized = &p.At
	case domain.PaymentSucceeded:
		succeeded = &p.At
	case domain.PaymentFailed:
		failed = &p.At
		if p.Reason != "" {
			reason = &p.Reason
		}
	}
	var d time.Time
	err := tx.QueryRow(ctx, `
		INSERT INTO fact_payment (transaction_id, tenant_id, date_key, gateway, currency, amount_minor,
			authorized_at, succeeded_at, failed_at, failure_reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (transaction_id) DO UPDATE SET
			date_key = LEAST(fact_payment.date_key, EXCLUDED.date_key),
			amount_minor = GREATEST(fact_payment.amount_minor, EXCLUDED.amount_minor),
			authorized_at = COALESCE(fact_payment.authorized_at, EXCLUDED.authorized_at),
			succeeded_at = COALESCE(fact_payment.succeeded_at, EXCLUDED.succeeded_at),
			failed_at = COALESCE(fact_payment.failed_at, EXCLUDED.failed_at),
			failure_reason = COALESCE(fact_payment.failure_reason, EXCLUDED.failure_reason)
		RETURNING date_key`,
		p.TransactionID, p.TenantID, domain.Date(p.At), p.Gateway, p.Currency, p.AmountMinor,
		authorized, succeeded, failed, reason).Scan(&d)
	if err != nil {
		return d, fmt.Errorf("put payment fact: %w", err)
	}
	return d, nil
}

// PutCheckoutStep records that a checkout session reached state.
func (Facts) PutCheckoutStep(ctx context.Context, tx pgx.Tx, tenantID, sessionID, state string, at time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO fact_checkout_step (session_id, state, tenant_id, date_key, occurred_at)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING`,
		sessionID, state, tenantID, domain.Date(at), at)
	if err != nil {
		return fmt.Errorf("put checkout step: %w", err)
	}
	return nil
}

// PutCartEvent records a cart event.
func (Facts) PutCartEvent(ctx context.Context, tx pgx.Tx, c domain.CartEvent) error {
	var ut *string
	if c.UserType != "" {
		ut = &c.UserType
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO fact_cart_event (cart_id, event, tenant_id, date_key, cart_type, in_funnel, user_type, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT DO NOTHING`,
		c.CartID, c.Event, c.TenantID, domain.Date(c.At), c.CartType, c.InFunnel, ut, c.At)
	if err != nil {
		return fmt.Errorf("put cart event: %w", err)
	}
	return nil
}

// PutVisitor records a visitor session.
func (Facts) PutVisitor(ctx context.Context, tx pgx.Tx, tenantID, eventID string, at time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO fact_visitor_session (event_id, tenant_id, date_key, created_at)
		VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, eventID, tenantID, domain.Date(at), at)
	if err != nil {
		return fmt.Errorf("put visitor: %w", err)
	}
	return nil
}

// Pseudonymize replaces a user's ID on their orders with pseudonym and
// returns the orders changed. No aggregate holds user IDs.
func (Facts) Pseudonymize(ctx context.Context, tx pgx.Tx, tenantID, userID, pseudonym string) (int64, error) {
	tag, err := tx.Exec(ctx, `UPDATE fact_order SET user_id = $3 WHERE tenant_id = $1 AND user_id = $2`,
		tenantID, userID, pseudonym)
	if err != nil {
		return 0, fmt.Errorf("pseudonymize orders: %w", err)
	}
	return tag.RowsAffected(), nil
}

// InsertMilestone records a milestone and reports whether it is new.
func (Facts) InsertMilestone(ctx context.Context, tx pgx.Tx, tenantID string, m domain.Milestone, orderID string, at time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO milestones (tenant_id, milestone, threshold, currency, order_id, reached_at)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`,
		tenantID, m.Kind, m.Threshold, m.Currency, orderID, at)
	if err != nil {
		return false, fmt.Errorf("insert milestone: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// LockTenant serialises milestone checks for a shop until tx ends.
func (Facts) LockTenant(ctx context.Context, tx pgx.Tx, tenantID string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('yc-analytics:' || $1))`, tenantID); err != nil {
		return fmt.Errorf("lock tenant: %w", err)
	}
	return nil
}
