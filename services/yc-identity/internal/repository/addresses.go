package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/domain"
)

// Addresses persists the customer delivery address book (issue #32).
type Addresses struct{}

const addressColumns = `id::text, user_id::text, label, address, is_default, deleted_at,
	COALESCE(replaced_by_id::text, ''), created_at, updated_at`

func scanAddress(r pgx.Row) (domain.DeliveryAddress, error) {
	var a domain.DeliveryAddress
	var raw []byte
	err := r.Scan(&a.ID, &a.UserID, &a.Label, &raw, &a.IsDefault, &a.DeletedAt,
		&a.ReplacedByID, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return a, err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a.Address); err != nil {
			return a, fmt.Errorf("decode address: %w", err)
		}
	}
	return a, nil
}

// Get returns one address by id for userID (including soft-deleted).
func (Addresses) Get(ctx context.Context, tx pgx.Tx, userID, id string) (domain.DeliveryAddress, error) {
	if uuid.Validate(id) != nil {
		return domain.DeliveryAddress{}, ycerrors.NotFound("Address", id)
	}
	a, err := scanAddress(tx.QueryRow(ctx, `SELECT `+addressColumns+`
		FROM customer_delivery_addresses WHERE user_id = $1 AND id = $2`, userID, id))
	if dbkit.IsNoRows(err) {
		return domain.DeliveryAddress{}, ycerrors.NotFound("Address", id)
	}
	if err != nil {
		return domain.DeliveryAddress{}, fmt.Errorf("get address: %w", err)
	}
	return a, nil
}

// GetActive returns an active address for userID.
func (Addresses) GetActive(ctx context.Context, tx pgx.Tx, userID, id string) (domain.DeliveryAddress, error) {
	a, err := Addresses{}.Get(ctx, tx, userID, id)
	if err != nil {
		return a, err
	}
	if !a.Active() {
		return domain.DeliveryAddress{}, ycerrors.NotFound("Address", id)
	}
	return a, nil
}

// List returns addresses for userID. includeDeleted includes soft-retired rows.
// Active rows come first (default first, then created_at ASC); deleted follow by deleted_at DESC.
func (Addresses) List(ctx context.Context, tx pgx.Tx, userID string, includeDeleted bool) ([]domain.DeliveryAddress, error) {
	q := `SELECT ` + addressColumns + ` FROM customer_delivery_addresses WHERE user_id = $1`
	if !includeDeleted {
		q += ` AND deleted_at IS NULL`
	}
	q += ` ORDER BY CASE WHEN deleted_at IS NULL THEN 0 ELSE 1 END,
		CASE WHEN deleted_at IS NULL AND is_default THEN 0 ELSE 1 END,
		created_at ASC, id ASC`
	rows, err := tx.Query(ctx, q, userID)
	if err != nil {
		return nil, fmt.Errorf("list addresses: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.DeliveryAddress, error) {
		return scanAddress(r)
	})
	if err != nil {
		return nil, fmt.Errorf("list addresses: %w", err)
	}
	return out, nil
}

// CountActive returns how many non-deleted addresses the user has.
func (Addresses) CountActive(ctx context.Context, tx pgx.Tx, userID string) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM customer_delivery_addresses
		WHERE user_id = $1 AND deleted_at IS NULL`, userID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count addresses: %w", err)
	}
	return n, nil
}

// Insert creates a new active address row.
func (Addresses) Insert(ctx context.Context, tx pgx.Tx, tenantID, userID string, a domain.DeliveryAddress) error {
	raw, err := json.Marshal(a.Address)
	if err != nil {
		return fmt.Errorf("encode address: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO customer_delivery_addresses
		(id, tenant_id, user_id, label, address, is_default, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7)`,
		a.ID, tenantID, userID, a.Label, raw, a.IsDefault, a.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert address: %w", err)
	}
	return nil
}

// ClearDefault clears is_default on the user's active default.
func (Addresses) ClearDefault(ctx context.Context, tx pgx.Tx, userID string, now time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE customer_delivery_addresses SET is_default = FALSE, updated_at = $2
		WHERE user_id = $1 AND deleted_at IS NULL AND is_default`, userID, now)
	if err != nil {
		return fmt.Errorf("clear default address: %w", err)
	}
	return nil
}

// SoftDelete marks an address retired and optionally links a successor.
func (Addresses) SoftDelete(ctx context.Context, tx pgx.Tx, userID, id, replacedByID string, now time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE customer_delivery_addresses
		SET deleted_at = $3, is_default = FALSE, replaced_by_id = NULLIF($4, '')::uuid, updated_at = $3
		WHERE user_id = $1 AND id = $2 AND deleted_at IS NULL`,
		userID, id, now, replacedByID)
	if err != nil {
		return fmt.Errorf("soft-delete address: %w", err)
	}
	return nil
}

// PromoteOldestDefault sets the earliest active address as default.
func (Addresses) PromoteOldestDefault(ctx context.Context, tx pgx.Tx, userID string, now time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE customer_delivery_addresses SET is_default = TRUE, updated_at = $2
		WHERE id = (
			SELECT id FROM customer_delivery_addresses
			WHERE user_id = $1 AND deleted_at IS NULL
			ORDER BY created_at ASC, id ASC LIMIT 1
		)`, userID, now)
	if err != nil {
		return fmt.Errorf("promote default address: %w", err)
	}
	return nil
}

// DeleteAll removes every address for userID (GDPR account deletion).
func (Addresses) DeleteAll(ctx context.Context, tx pgx.Tx, userID string) error {
	_, err := tx.Exec(ctx, `DELETE FROM customer_delivery_addresses WHERE user_id = $1`, userID)
	if err != nil {
		return fmt.Errorf("delete addresses: %w", err)
	}
	return nil
}
