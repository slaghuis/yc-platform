// Package repository is yc-orders' Postgres access. Every method runs in
// the caller's transaction, so RLS (app.tenant_id) scopes the rows.
package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"

	"github.com/slaghuis/yc-platform/services/yc-orders/internal/domain"
)

// Orders reads and writes the order tables.
type Orders struct{}

const orderColumns = `id, COALESCE(order_number, ''), checkout_session_id, user_id, user_type, guest_contact,
	delivery_address, requires_shipping, status, items, pricing_snapshot, shipping_snapshot, currency,
	items_total_minor, shipping_minor, tax_total_minor, total_minor, COALESCE(payment_transaction_id::text, ''),
	COALESCE(payment_gateway, ''), refunded_minor, created_at, updated_at, placed_at`

func scanOrder(r pgx.Row) (domain.Order, error) {
	var o domain.Order
	var userType, status string
	var guest, address, items []byte
	err := r.Scan(&o.ID, &o.OrderNumber, &o.CheckoutSessionID, &o.UserID, &userType, &guest,
		&address, &o.RequiresShipping, &status, &items, &o.PricingSnapshot, &o.ShippingSnapshot, &o.Currency,
		&o.ItemsTotalMinor, &o.ShippingMinor, &o.TaxTotalMinor, &o.TotalMinor, &o.PaymentTransactionID,
		&o.PaymentGateway, &o.RefundedMinor, &o.CreatedAt, &o.UpdatedAt, &o.PlacedAt)
	if err != nil {
		return o, err
	}
	o.UserType, o.Status = domain.UserType(userType), domain.Status(status)
	if len(guest) > 0 {
		if err := json.Unmarshal(guest, &o.GuestContact); err != nil {
			return o, fmt.Errorf("decode guest_contact: %w", err)
		}
	}
	if len(address) > 0 {
		if err := json.Unmarshal(address, &o.DeliveryAddress); err != nil {
			return o, fmt.Errorf("decode delivery_address: %w", err)
		}
	}
	if err := json.Unmarshal(items, &o.Items); err != nil {
		return o, fmt.Errorf("decode items: %w", err)
	}
	return o, nil
}

func (Orders) one(ctx context.Context, tx pgx.Tx, where string, args ...any) (domain.Order, bool, error) {
	o, err := scanOrder(tx.QueryRow(ctx, `SELECT `+orderColumns+` FROM orders WHERE `+where, args...))
	if dbkit.IsNoRows(err) {
		return domain.Order{}, false, nil
	}
	if err != nil {
		return domain.Order{}, false, fmt.Errorf("get order: %w", err)
	}
	return o, true, nil
}

// Order returns an order; forUpdate locks it.
func (r Orders) Order(ctx context.Context, tx pgx.Tx, tenantID, id string, forUpdate bool) (domain.Order, bool, error) {
	if !validUUID(id) {
		return domain.Order{}, false, nil
	}
	q := `tenant_id = $1 AND id = $2`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	return r.one(ctx, tx, q, tenantID, id)
}

// OrderBySession returns the order a checkout session created.
func (r Orders) OrderBySession(ctx context.Context, tx pgx.Tx, tenantID, sessionID string) (domain.Order, bool, error) {
	return r.one(ctx, tx, `tenant_id = $1 AND checkout_session_id = $2`, tenantID, sessionID)
}

func nullJSON(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

// InsertOrder records a new order.
func (Orders) InsertOrder(ctx context.Context, tx pgx.Tx, tenantID string, o domain.Order) error {
	var guest, address []byte
	var err error
	if o.GuestContact != nil {
		if guest, err = json.Marshal(o.GuestContact); err != nil {
			return fmt.Errorf("encode guest_contact: %w", err)
		}
	}
	if len(o.DeliveryAddress) > 0 {
		if address, err = nullJSON(o.DeliveryAddress); err != nil {
			return fmt.Errorf("encode delivery_address: %w", err)
		}
	}
	items, err := json.Marshal(o.Items)
	if err != nil {
		return fmt.Errorf("encode items: %w", err)
	}
	var shipping []byte
	if len(o.ShippingSnapshot) > 0 {
		shipping = o.ShippingSnapshot
	}
	_, err = tx.Exec(ctx, `INSERT INTO orders (id, tenant_id, checkout_session_id, user_id, user_type, guest_contact,
			source_email, delivery_address, requires_shipping, status, items, pricing_snapshot, shipping_snapshot,
			currency, items_total_minor, shipping_minor, tax_total_minor, total_minor, payment_transaction_id,
			payment_gateway, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18,
			NULLIF($19, '')::uuid, NULLIF($20, ''), $21, $21)`,
		o.ID, tenantID, o.CheckoutSessionID, o.UserID, string(o.UserType), guest,
		o.SourceEmail(), address, o.RequiresShipping, string(o.Status), items, []byte(o.PricingSnapshot), shipping,
		o.Currency, o.ItemsTotalMinor, o.ShippingMinor, o.TaxTotalMinor, o.TotalMinor, o.PaymentTransactionID,
		o.PaymentGateway, o.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert order: %w", err)
	}
	return nil
}

// UpdateOrder saves an order's status, number, payment and refund.
func (Orders) UpdateOrder(ctx context.Context, tx pgx.Tx, tenantID string, o domain.Order) error {
	_, err := tx.Exec(ctx, `UPDATE orders SET status = $3, order_number = NULLIF($4, ''),
			payment_transaction_id = NULLIF($5, '')::uuid, refunded_minor = $6, updated_at = $7, placed_at = $8
		WHERE tenant_id = $1 AND id = $2`,
		tenantID, o.ID, string(o.Status), o.OrderNumber, o.PaymentTransactionID, o.RefundedMinor, o.UpdatedAt, o.PlacedAt)
	if err != nil {
		return fmt.Errorf("update order: %w", err)
	}
	return nil
}

// NextNumber takes the shop's next order number; the counter row's lock
// serialises concurrent confirmations.
func (Orders) NextNumber(ctx context.Context, tx pgx.Tx, tenantID string) (int64, error) {
	var n int64
	err := tx.QueryRow(ctx, `INSERT INTO order_counters (tenant_id, last_number) VALUES ($1, 1)
		ON CONFLICT (tenant_id) DO UPDATE SET last_number = order_counters.last_number + 1
		RETURNING last_number`, tenantID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("next order number: %w", err)
	}
	return n, nil
}

// AddHistory records a status change.
func (Orders) AddHistory(ctx context.Context, tx pgx.Tx, tenantID, orderID string, h domain.HistoryEntry) error {
	_, err := tx.Exec(ctx, `INSERT INTO order_status_history (order_id, tenant_id, from_status, to_status, actor_id,
			reason, created_at)
		VALUES ($1, $2, NULLIF($3, ''), $4, NULLIF($5, ''), NULLIF($6, ''), $7)`,
		orderID, tenantID, string(h.From), string(h.To), h.ActorID, h.Reason, h.CreatedAt)
	if err != nil {
		return fmt.Errorf("add order history: %w", err)
	}
	return nil
}

// History returns an order's status changes, oldest first.
func (Orders) History(ctx context.Context, tx pgx.Tx, tenantID, orderID string) ([]domain.HistoryEntry, error) {
	rows, err := tx.Query(ctx, `SELECT COALESCE(from_status, ''), to_status, COALESCE(actor_id, ''), COALESCE(reason, ''),
			created_at
		FROM order_status_history WHERE tenant_id = $1 AND order_id = $2 ORDER BY created_at, id`, tenantID, orderID)
	if err != nil {
		return nil, fmt.Errorf("list order history: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.HistoryEntry, error) {
		var h domain.HistoryEntry
		var from, to string
		err := r.Scan(&from, &to, &h.ActorID, &h.Reason, &h.CreatedAt)
		h.From, h.To = domain.Status(from), domain.Status(to)
		return h, err
	})
	if err != nil {
		return nil, fmt.Errorf("list order history: %w", err)
	}
	return out, nil
}

// OrderFilter narrows ListOrders.
type OrderFilter struct {
	UserID string
	// Statuses limits the list; empty means placed orders.
	Statuses []domain.Status
}

// OrderCursor is the sort key of the last order on a page; lists run
// newest first.
type OrderCursor struct {
	At time.Time `json:"at"`
	ID string    `json:"id"`
}

// ListOrders returns up to limit orders, newest first.
func (r Orders) ListOrders(ctx context.Context, tx pgx.Tx, tenantID string, f OrderFilter, after *OrderCursor, limit int) ([]domain.Order, error) {
	statuses := make([]string, 0, len(f.Statuses))
	for _, s := range f.Statuses {
		statuses = append(statuses, string(s))
	}
	if len(statuses) == 0 {
		statuses = []string{string(domain.StatusReceived), string(domain.StatusCancelled)}
	}
	args := []any{tenantID, f.UserID, statuses, limit}
	q := `SELECT ` + orderColumns + ` FROM orders WHERE tenant_id = $1 AND ($2 = '' OR user_id = $2)
		AND status = ANY($3::text[])`
	if after != nil {
		q += ` AND (created_at, id) < ($5, $6)`
		args = append(args, after.At, after.ID)
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT $4`
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list orders: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Order, error) { return scanOrder(r) })
	if err != nil {
		return nil, fmt.Errorf("list orders: %w", err)
	}
	return out, nil
}

func validUUID(s string) bool { return uuid.Validate(s) == nil }
