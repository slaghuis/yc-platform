// Package repository is yc-inventory's Postgres access. Every method runs
// in the caller's transaction, so RLS (app.tenant_id) scopes the rows.
package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	"github.com/slaghuis/yc-platform/services/yc-inventory/internal/domain"
)

// Stock reads and writes the inventory tables.
type Stock struct{}

// Config returns tenantID's settings, or false when it has none.
func (Stock) Config(ctx context.Context, tx pgx.Tx, tenantID string) (domain.Config, bool, error) {
	c := domain.Config{Stored: true}
	err := tx.QueryRow(ctx, `SELECT inventory_enabled, default_reservation_ttl_minutes, min_reservation_ttl_minutes,
			max_reservation_ttl_minutes, updated_at
		FROM tenant_inventory_config WHERE tenant_id = $1`, tenantID).
		Scan(&c.Enabled, &c.DefaultTTL, &c.MinTTL, &c.MaxTTL, &c.UpdatedAt)
	if dbkit.IsNoRows(err) {
		return domain.Config{}, false, nil
	}
	if err != nil {
		return domain.Config{}, false, fmt.Errorf("get inventory config: %w", err)
	}
	return c, true, nil
}

// PutConfig creates or replaces tenantID's settings; with onlyNew it keeps
// existing ones.
func (Stock) PutConfig(ctx context.Context, tx pgx.Tx, tenantID string, c domain.Config, now time.Time, onlyNew bool) error {
	q := `INSERT INTO tenant_inventory_config (tenant_id, inventory_enabled, default_reservation_ttl_minutes,
			min_reservation_ttl_minutes, max_reservation_ttl_minutes, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6) ON CONFLICT (tenant_id) `
	if onlyNew {
		q += `DO NOTHING`
	} else {
		q += `DO UPDATE SET inventory_enabled = EXCLUDED.inventory_enabled,
			default_reservation_ttl_minutes = EXCLUDED.default_reservation_ttl_minutes,
			min_reservation_ttl_minutes = EXCLUDED.min_reservation_ttl_minutes,
			max_reservation_ttl_minutes = EXCLUDED.max_reservation_ttl_minutes, updated_at = EXCLUDED.updated_at`
	}
	if _, err := tx.Exec(ctx, q, tenantID, c.Enabled, c.DefaultTTL, c.MinTTL, c.MaxTTL, now); err != nil {
		return fmt.Errorf("put inventory config: %w", err)
	}
	return nil
}

const levelColumns = `sku_id, on_hand, reserved, low_threshold, created_at, updated_at`

func scanLevel(r pgx.Row) (domain.Level, error) {
	var l domain.Level
	err := r.Scan(&l.SKUID, &l.OnHand, &l.Reserved, &l.LowThreshold, &l.CreatedAt, &l.UpdatedAt)
	return l, err
}

// Level returns a SKU's stock, locked when forUpdate, or false when the SKU
// is not tracked.
func (Stock) Level(ctx context.Context, tx pgx.Tx, tenantID, skuID string, forUpdate bool) (domain.Level, bool, error) {
	q := `SELECT ` + levelColumns + ` FROM stock_levels WHERE tenant_id = $1 AND sku_id = $2`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	l, err := scanLevel(tx.QueryRow(ctx, q, tenantID, skuID))
	if dbkit.IsNoRows(err) {
		return domain.Level{}, false, nil
	}
	if err != nil {
		return domain.Level{}, false, fmt.Errorf("get stock level: %w", err)
	}
	return l, true, nil
}

// LockLevels locks the levels of skuIDs in SKU order, so concurrent
// reservations cannot deadlock. Untracked SKUs are absent from the map.
func (Stock) LockLevels(ctx context.Context, tx pgx.Tx, tenantID string, skuIDs []string) (map[string]domain.Level, error) {
	rows, err := tx.Query(ctx, `SELECT `+levelColumns+` FROM stock_levels
		WHERE tenant_id = $1 AND sku_id = ANY($2::uuid[]) ORDER BY sku_id FOR UPDATE`, tenantID, skuIDs)
	if err != nil {
		return nil, fmt.Errorf("lock stock levels: %w", err)
	}
	levels, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Level, error) { return scanLevel(r) })
	if err != nil {
		return nil, fmt.Errorf("lock stock levels: %w", err)
	}
	out := make(map[string]domain.Level, len(levels))
	for _, l := range levels {
		out[l.SKUID] = l
	}
	return out, nil
}

// InsertLevel starts tracking a SKU at zero, reporting whether it was new.
func (Stock) InsertLevel(ctx context.Context, tx pgx.Tx, tenantID, skuID string, now time.Time) (domain.Level, bool, error) {
	l, err := scanLevel(tx.QueryRow(ctx, `
		INSERT INTO stock_levels (tenant_id, sku_id, created_at, updated_at) VALUES ($1, $2, $3, $3)
		ON CONFLICT (tenant_id, sku_id) DO NOTHING RETURNING `+levelColumns, tenantID, skuID, now))
	if dbkit.IsNoRows(err) {
		return domain.Level{}, false, nil
	}
	if err != nil {
		return domain.Level{}, false, fmt.Errorf("insert stock level: %w", err)
	}
	return l, true, nil
}

// DeleteLevel stops tracking a SKU, returning its last level.
func (Stock) DeleteLevel(ctx context.Context, tx pgx.Tx, tenantID, skuID string) (domain.Level, bool, error) {
	l, err := scanLevel(tx.QueryRow(ctx, `DELETE FROM stock_levels WHERE tenant_id = $1 AND sku_id = $2
		RETURNING `+levelColumns, tenantID, skuID))
	if dbkit.IsNoRows(err) {
		return domain.Level{}, false, nil
	}
	if err != nil {
		return domain.Level{}, false, fmt.Errorf("delete stock level: %w", err)
	}
	return l, true, nil
}

// UpdateLevel writes a level's counts and threshold.
func (Stock) UpdateLevel(ctx context.Context, tx pgx.Tx, tenantID string, l domain.Level, now time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE stock_levels SET on_hand = $3, reserved = $4, low_threshold = $5, updated_at = $6
		WHERE tenant_id = $1 AND sku_id = $2`, tenantID, l.SKUID, l.OnHand, l.Reserved, l.LowThreshold, now)
	if err != nil {
		return fmt.Errorf("update stock level: %w", err)
	}
	return nil
}

// LevelFilter narrows ListLevels.
type LevelFilter struct {
	BelowThreshold bool // available under the low threshold
	OutOfStock     bool // available at or below zero
}

// ListLevels returns up to limit levels after the SKU ID after, by SKU.
func (Stock) ListLevels(ctx context.Context, tx pgx.Tx, tenantID string, f LevelFilter, after string, limit int) ([]domain.Level, error) {
	q := `SELECT ` + levelColumns + ` FROM stock_levels WHERE tenant_id = $1 AND ($2 = '' OR sku_id > NULLIF($2, '')::uuid)`
	if f.BelowThreshold {
		q += ` AND low_threshold > 0 AND on_hand - reserved < low_threshold`
	}
	if f.OutOfStock {
		q += ` AND on_hand - reserved <= 0`
	}
	q += ` ORDER BY sku_id LIMIT $3`
	rows, err := tx.Query(ctx, q, tenantID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list stock levels: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Level, error) { return scanLevel(r) })
	if err != nil {
		return nil, fmt.Errorf("list stock levels: %w", err)
	}
	return out, nil
}

// AddMovement records a change to a level.
func (Stock) AddMovement(ctx context.Context, tx pgx.Tx, tenantID string, m domain.Movement) error {
	_, err := tx.Exec(ctx, `INSERT INTO stock_movements (tenant_id, sku_id, movement_type, on_hand_delta, reserved_delta,
			reason, actor_id, reference, created_at)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), NULLIF($7, ''), NULLIF($8, ''), $9)`,
		tenantID, m.SKUID, string(m.Type), m.OnHandDelta, m.ReservedDelta, m.Reason, m.ActorID, m.Reference, m.CreatedAt)
	if err != nil {
		return fmt.Errorf("add stock movement: %w", err)
	}
	return nil
}

// MovementCursor is the sort key of the last movement on a page; history
// runs newest first.
type MovementCursor struct {
	At time.Time `json:"at"`
	ID string    `json:"id"`
}

// Movements returns up to limit of a SKU's movements, newest first.
func (Stock) Movements(ctx context.Context, tx pgx.Tx, tenantID, skuID string, after *MovementCursor, limit int) ([]domain.Movement, error) {
	args := []any{tenantID, skuID, limit}
	q := `SELECT id, sku_id, movement_type, on_hand_delta, reserved_delta, COALESCE(reason, ''), COALESCE(actor_id, ''),
			COALESCE(reference, ''), created_at
		FROM stock_movements WHERE tenant_id = $1 AND sku_id = $2`
	if after != nil {
		q += ` AND (created_at, id) < ($4, $5::uuid)`
		args = append(args, after.At, after.ID)
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT $3`
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list stock movements: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Movement, error) {
		var m domain.Movement
		err := r.Scan(&m.ID, &m.SKUID, &m.Type, &m.OnHandDelta, &m.ReservedDelta, &m.Reason, &m.ActorID, &m.Reference, &m.CreatedAt)
		return m, err
	})
	if err != nil {
		return nil, fmt.Errorf("list stock movements: %w", err)
	}
	return out, nil
}

const reservationColumns = `id, session_id, idempotency_key, status, expires_at, created_at, settled_at`

func (Stock) scanReservation(ctx context.Context, tx pgx.Tx, row pgx.Row) (domain.Reservation, bool, error) {
	var r domain.Reservation
	err := row.Scan(&r.ID, &r.SessionID, &r.IdempotencyKey, &r.Status, &r.ExpiresAt, &r.CreatedAt, &r.SettledAt)
	if dbkit.IsNoRows(err) {
		return domain.Reservation{}, false, nil
	}
	if err != nil {
		return domain.Reservation{}, false, fmt.Errorf("get reservation: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT sku_id, quantity FROM reservation_items WHERE reservation_id = $1 ORDER BY sku_id`, r.ID)
	if err != nil {
		return domain.Reservation{}, false, fmt.Errorf("get reservation items: %w", err)
	}
	r.Items, err = pgx.CollectRows(rows, pgx.RowToStructByPos[domain.Line])
	if err != nil {
		return domain.Reservation{}, false, fmt.Errorf("get reservation items: %w", err)
	}
	return r, true, nil
}

// Reservation returns a reservation with its items, locked when
// forUpdate, or false when there is none.
func (s Stock) Reservation(ctx context.Context, tx pgx.Tx, tenantID, id string, forUpdate bool) (domain.Reservation, bool, error) {
	q := `SELECT ` + reservationColumns + ` FROM reservations WHERE tenant_id = $1 AND id = $2`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	return s.scanReservation(ctx, tx, tx.QueryRow(ctx, q, tenantID, id))
}

// ReservationByKey returns the reservation made with an idempotency key.
func (s Stock) ReservationByKey(ctx context.Context, tx pgx.Tx, tenantID, key string) (domain.Reservation, bool, error) {
	return s.scanReservation(ctx, tx, tx.QueryRow(ctx, `SELECT `+reservationColumns+` FROM reservations
		WHERE tenant_id = $1 AND idempotency_key = $2`, tenantID, key))
}

// InsertReservation stores a new reservation and its items.
func (Stock) InsertReservation(ctx context.Context, tx pgx.Tx, tenantID string, r domain.Reservation) error {
	_, err := tx.Exec(ctx, `INSERT INTO reservations (id, tenant_id, session_id, idempotency_key, status, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		r.ID, tenantID, r.SessionID, r.IdempotencyKey, string(r.Status), r.ExpiresAt, r.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert reservation: %w", err)
	}
	for _, it := range r.Items {
		_, err := tx.Exec(ctx, `INSERT INTO reservation_items (reservation_id, tenant_id, sku_id, quantity)
			VALUES ($1, $2, $3, $4)`, r.ID, tenantID, it.SKUID, it.Quantity)
		if err != nil {
			return fmt.Errorf("insert reservation item: %w", err)
		}
	}
	return nil
}

// SettleReservation moves a reservation to a final status.
func (Stock) SettleReservation(ctx context.Context, tx pgx.Tx, tenantID, id string, status domain.ReservationStatus, now time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE reservations SET status = $3, settled_at = $4 WHERE tenant_id = $1 AND id = $2`,
		tenantID, id, string(status), now)
	if err != nil {
		return fmt.Errorf("settle reservation: %w", err)
	}
	return nil
}

// ActiveReservations returns a SKU's active reservations, soonest expiry
// first.
func (s Stock) ActiveReservations(ctx context.Context, tx pgx.Tx, tenantID, skuID string) ([]domain.Reservation, error) {
	rows, err := tx.Query(ctx, `SELECT r.id FROM reservations r
		JOIN reservation_items i ON i.reservation_id = r.id
		WHERE r.tenant_id = $1 AND i.sku_id = $2 AND r.status = 'active' ORDER BY r.expires_at, r.id`, tenantID, skuID)
	if err != nil {
		return nil, fmt.Errorf("list reservations: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list reservations: %w", err)
	}
	out := make([]domain.Reservation, 0, len(ids))
	for _, id := range ids {
		r, _, err := s.Reservation(ctx, tx, tenantID, id, false)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// DueReservation identifies an active reservation past its expiry.
type DueReservation struct {
	TenantID string
	ID       string
}

// DueReservations returns up to limit active reservations that expired by
// now, across tenants (platform transaction).
func (Stock) DueReservations(ctx context.Context, tx pgx.Tx, now time.Time, limit int) ([]DueReservation, error) {
	rows, err := tx.Query(ctx, `SELECT tenant_id, id FROM reservations
		WHERE status = 'active' AND expires_at <= $1 ORDER BY expires_at LIMIT $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list due reservations: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[DueReservation])
	if err != nil {
		return nil, fmt.Errorf("list due reservations: %w", err)
	}
	return out, nil
}
