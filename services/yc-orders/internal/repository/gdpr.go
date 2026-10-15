package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"

	"github.com/slaghuis/yc-platform/services/yc-orders/internal/domain"
)

// CustomerOrder is one of a customer's orders, for GDPR checks.
type CustomerOrder struct {
	ID     string
	Status domain.Status
	// OpenReturn is whether a return awaits the shop's decision.
	OpenReturn bool
}

// MaxCustomerOrders bounds CustomerOrders.
const MaxCustomerOrders = 1000

// CustomerOrders returns the orders placed or started by any of userIDs,
// newest first.
func (Orders) CustomerOrders(ctx context.Context, tx pgx.Tx, tenantID string, userIDs []string) ([]CustomerOrder, error) {
	rows, err := tx.Query(ctx, `SELECT o.id::text, o.status, EXISTS (SELECT 1 FROM order_returns r
			WHERE r.tenant_id = o.tenant_id AND r.order_id = o.id AND r.status = 'requested')
		FROM orders o WHERE o.tenant_id = $1 AND o.user_id = ANY($2::text[])
		ORDER BY o.created_at DESC, o.id DESC LIMIT $3`, tenantID, userIDs, MaxCustomerOrders)
	if err != nil {
		return nil, fmt.Errorf("customer orders: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CustomerOrder, error) {
		var c CustomerOrder
		var status string
		err := r.Scan(&c.ID, &status, &c.OpenReturn)
		c.Status = domain.Status(status)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("customer orders: %w", err)
	}
	return out, nil
}

// RecordDeletion remembers that userID was deleted as pseudonym. It
// reports false when the deletion was already recorded.
func (Orders) RecordDeletion(ctx context.Context, tx pgx.Tx, tenantID, userID, pseudonym string, at time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, `INSERT INTO deleted_customers (tenant_id, user_id, pseudonym, deleted_at)
		VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, tenantID, userID, pseudonym, at)
	if err != nil {
		return false, fmt.Errorf("record deletion: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Pseudonym returns a deleted customer's pseudonym, or false.
func (Orders) Pseudonym(ctx context.Context, tx pgx.Tx, tenantID, userID string) (string, bool, error) {
	var p string
	err := tx.QueryRow(ctx, `SELECT pseudonym FROM deleted_customers WHERE tenant_id = $1 AND user_id = $2`,
		tenantID, userID).Scan(&p)
	if dbkit.IsNoRows(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get pseudonym: %w", err)
	}
	return p, true, nil
}

// Pseudonymise replaces userID with pseudonym on its orders, returns and
// status history and clears the orders' addresses and contacts. It
// returns how many orders changed.
func (Orders) Pseudonymise(ctx context.Context, tx pgx.Tx, tenantID, userID, pseudonym string, at time.Time) (int64, error) {
	tag, err := tx.Exec(ctx, `UPDATE orders SET user_id = $3, delivery_address = NULL, guest_contact = NULL,
			source_email = NULL, updated_at = $4
		WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID, pseudonym, at)
	if err != nil {
		return 0, fmt.Errorf("pseudonymise orders: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE order_returns SET requested_by = $3 WHERE tenant_id = $1 AND requested_by = $2`,
		tenantID, userID, pseudonym); err != nil {
		return 0, fmt.Errorf("pseudonymise returns: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE order_status_history SET actor_id = $3 WHERE tenant_id = $1 AND actor_id = $2`,
		tenantID, userID, pseudonym); err != nil {
		return 0, fmt.Errorf("pseudonymise order history: %w", err)
	}
	return tag.RowsAffected(), nil
}
