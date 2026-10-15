package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"

	"github.com/slaghuis/yc-platform/services/yc-orders/internal/domain"
)

const returnColumns = `id, order_id, status, items, COALESCE(reason, ''), refund_shipping, refund_minor,
	COALESCE(decision_reason, ''), requested_by, COALESCE(decided_by, ''), requested_at, decided_at`

func scanReturn(r pgx.Row) (domain.Return, error) {
	var ret domain.Return
	var status string
	var items []byte
	err := r.Scan(&ret.ID, &ret.OrderID, &status, &items, &ret.Reason, &ret.RefundShipping, &ret.RefundMinor,
		&ret.DecisionReason, &ret.RequestedBy, &ret.DecidedBy, &ret.RequestedAt, &ret.DecidedAt)
	if err != nil {
		return ret, err
	}
	ret.Status = domain.ReturnStatus(status)
	if err := json.Unmarshal(items, &ret.Items); err != nil {
		return ret, fmt.Errorf("decode return items: %w", err)
	}
	return ret, nil
}

// InsertReturn records a return request. An order has one open request:
// a second is a unique violation.
func (Orders) InsertReturn(ctx context.Context, tx pgx.Tx, tenantID string, r domain.Return) error {
	items, err := json.Marshal(r.Items)
	if err != nil {
		return fmt.Errorf("encode return items: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO order_returns (id, tenant_id, order_id, status, items, reason, requested_by,
			requested_at)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7, $8)`,
		r.ID, tenantID, r.OrderID, string(r.Status), items, r.Reason, r.RequestedBy, r.RequestedAt)
	if err != nil {
		return fmt.Errorf("insert return: %w", err)
	}
	return nil
}

// UpdateReturn saves a return's decision.
func (Orders) UpdateReturn(ctx context.Context, tx pgx.Tx, tenantID string, r domain.Return) error {
	_, err := tx.Exec(ctx, `UPDATE order_returns SET status = $3, refund_shipping = $4, refund_minor = $5,
			decision_reason = NULLIF($6, ''), decided_by = NULLIF($7, ''), decided_at = $8
		WHERE tenant_id = $1 AND id = $2`,
		tenantID, r.ID, string(r.Status), r.RefundShipping, r.RefundMinor, r.DecisionReason, r.DecidedBy, r.DecidedAt)
	if err != nil {
		return fmt.Errorf("update return: %w", err)
	}
	return nil
}

// Return returns a return request; forUpdate locks it.
func (Orders) Return(ctx context.Context, tx pgx.Tx, tenantID, id string, forUpdate bool) (domain.Return, bool, error) {
	if !validUUID(id) {
		return domain.Return{}, false, nil
	}
	q := `SELECT ` + returnColumns + ` FROM order_returns WHERE tenant_id = $1 AND id = $2`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	r, err := scanReturn(tx.QueryRow(ctx, q, tenantID, id))
	if dbkit.IsNoRows(err) {
		return domain.Return{}, false, nil
	}
	if err != nil {
		return domain.Return{}, false, fmt.Errorf("get return: %w", err)
	}
	return r, true, nil
}

// Returns lists an order's returns, oldest first.
func (Orders) Returns(ctx context.Context, tx pgx.Tx, tenantID, orderID string) ([]domain.Return, error) {
	rows, err := tx.Query(ctx, `SELECT `+returnColumns+` FROM order_returns
		WHERE tenant_id = $1 AND order_id = $2 ORDER BY requested_at, id`, tenantID, orderID)
	if err != nil {
		return nil, fmt.Errorf("list returns: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Return, error) { return scanReturn(r) })
	if err != nil {
		return nil, fmt.Errorf("list returns: %w", err)
	}
	return out, nil
}

// ReturnCursor is the sort key of the last return on a page; lists run
// newest first.
type ReturnCursor struct {
	At time.Time `json:"at"`
	ID string    `json:"id"`
}

// ListReturns returns up to limit of the shop's returns, newest first,
// optionally in one status.
func (Orders) ListReturns(ctx context.Context, tx pgx.Tx, tenantID string, status domain.ReturnStatus, after *ReturnCursor, limit int) ([]domain.Return, error) {
	args := []any{tenantID, string(status), limit}
	q := `SELECT ` + returnColumns + ` FROM order_returns WHERE tenant_id = $1 AND ($2 = '' OR status = $2)`
	if after != nil {
		q += ` AND (requested_at, id) < ($4, $5)`
		args = append(args, after.At, after.ID)
	}
	q += ` ORDER BY requested_at DESC, id DESC LIMIT $3`
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list returns: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Return, error) { return scanReturn(r) })
	if err != nil {
		return nil, fmt.Errorf("list returns: %w", err)
	}
	return out, nil
}

const refundColumns = `id, COALESCE(return_id::text, ''), amount_minor, status, reason, payment_key,
	COALESCE(actor_id, ''), created_at`

func scanRefund(r pgx.Row) (domain.Refund, error) {
	var f domain.Refund
	err := r.Scan(&f.ID, &f.ReturnID, &f.AmountMinor, &f.Status, &f.Reason, &f.PaymentKey, &f.ActorID, &f.CreatedAt)
	return f, err
}

// InsertRefund records a refund.
func (Orders) InsertRefund(ctx context.Context, tx pgx.Tx, tenantID, orderID string, f domain.Refund) error {
	_, err := tx.Exec(ctx, `INSERT INTO order_refunds (id, tenant_id, order_id, return_id, amount_minor, status,
			reason, payment_key, actor_id, created_at)
		VALUES ($1, $2, $3, NULLIF($4, '')::uuid, $5, $6, $7, $8, NULLIF($9, ''), $10)`,
		f.ID, tenantID, orderID, f.ReturnID, f.AmountMinor, f.Status, f.Reason, f.PaymentKey, f.ActorID, f.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert refund: %w", err)
	}
	return nil
}

// RefundByKey returns the refund recorded for a payment key.
func (Orders) RefundByKey(ctx context.Context, tx pgx.Tx, tenantID, key string) (domain.Refund, bool, error) {
	f, err := scanRefund(tx.QueryRow(ctx, `SELECT `+refundColumns+` FROM order_refunds
		WHERE tenant_id = $1 AND payment_key = $2`, tenantID, key))
	if dbkit.IsNoRows(err) {
		return domain.Refund{}, false, nil
	}
	if err != nil {
		return domain.Refund{}, false, fmt.Errorf("get refund: %w", err)
	}
	return f, true, nil
}

// Refunds lists an order's refunds, oldest first.
func (Orders) Refunds(ctx context.Context, tx pgx.Tx, tenantID, orderID string) ([]domain.Refund, error) {
	rows, err := tx.Query(ctx, `SELECT `+refundColumns+` FROM order_refunds
		WHERE tenant_id = $1 AND order_id = $2 ORDER BY created_at, id`, tenantID, orderID)
	if err != nil {
		return nil, fmt.Errorf("list refunds: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Refund, error) { return scanRefund(r) })
	if err != nil {
		return nil, fmt.Errorf("list refunds: %w", err)
	}
	return out, nil
}
