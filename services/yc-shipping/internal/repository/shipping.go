// Package repository is yc-shipping's Postgres access. Every method runs in
// the caller's transaction, so RLS (app.tenant_id) scopes the rows.
package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	"github.com/slaghuis/yc-platform/services/yc-shipping/internal/domain"
)

// Shipping reads and writes the shipping tables.
type Shipping struct{}

const zoneColumns = `id, name, match_rules, priority, created_at, updated_at`

func scanZone(r pgx.Row) (domain.Zone, error) {
	var z domain.Zone
	var rules []byte
	if err := r.Scan(&z.ID, &z.Name, &rules, &z.Priority, &z.CreatedAt, &z.UpdatedAt); err != nil {
		return z, err
	}
	if err := json.Unmarshal(rules, &z.Rules); err != nil {
		return z, fmt.Errorf("decode match rules: %w", err)
	}
	return z, nil
}

// Zones returns the shop's zones in matching order.
func (Shipping) Zones(ctx context.Context, tx pgx.Tx, tenantID string) ([]domain.Zone, error) {
	rows, err := tx.Query(ctx, `SELECT `+zoneColumns+` FROM zones WHERE tenant_id = $1
		ORDER BY priority, name, id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list zones: %w", err)
	}
	zones, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Zone, error) { return scanZone(r) })
	if err != nil {
		return nil, fmt.Errorf("list zones: %w", err)
	}
	return zones, nil
}

// Zone returns one zone, or false.
func (Shipping) Zone(ctx context.Context, tx pgx.Tx, tenantID, id string) (domain.Zone, bool, error) {
	z, err := scanZone(tx.QueryRow(ctx, `SELECT `+zoneColumns+` FROM zones WHERE tenant_id = $1 AND id = $2`, tenantID, id))
	if dbkit.IsNoRows(err) {
		return z, false, nil
	}
	if err != nil {
		return z, false, fmt.Errorf("get zone: %w", err)
	}
	return z, true, nil
}

// PutZone creates or replaces a zone.
func (Shipping) PutZone(ctx context.Context, tx pgx.Tx, tenantID string, z domain.Zone) error {
	rules, err := json.Marshal(z.Rules)
	if err != nil {
		return fmt.Errorf("encode match rules: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO zones (id, tenant_id, name, match_rules, priority, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, match_rules = EXCLUDED.match_rules,
			priority = EXCLUDED.priority, updated_at = EXCLUDED.updated_at`,
		z.ID, tenantID, z.Name, rules, z.Priority, z.CreatedAt, z.UpdatedAt)
	if err != nil {
		return fmt.Errorf("put zone: %w", err)
	}
	return nil
}

// DeleteZone deletes a zone and its rates, reporting whether it existed.
func (Shipping) DeleteZone(ctx context.Context, tx pgx.Tx, tenantID, id string) (bool, error) {
	tag, err := tx.Exec(ctx, `DELETE FROM zones WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return false, fmt.Errorf("delete zone: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

const rateColumns = `id, zone_id, method, display_name, pricing_type, flat_amount_minor, weight_tiers, price_tiers,
	estimated_days_min, estimated_days_max, carrier_driver, COALESCE(schedule_id::text, ''), requires_delivery_slot,
	status, created_at, updated_at`

func scanRate(r pgx.Row) (domain.Rate, error) {
	var (
		rt             domain.Rate
		weight, price  []byte
		status, ptype  string
		daysLo, daysHi *int
	)
	err := r.Scan(&rt.ID, &rt.ZoneID, &rt.Method, &rt.DisplayName, &ptype, &rt.FlatAmountMinor, &weight, &price,
		&daysLo, &daysHi, &rt.CarrierDriver, &rt.ScheduleID, &rt.RequiresDeliverySlot, &status, &rt.CreatedAt, &rt.UpdatedAt)
	if err != nil {
		return rt, err
	}
	rt.PricingType, rt.Active, rt.EstimatedDaysMin, rt.EstimatedDaysMax = domain.PricingType(ptype), status == "active", daysLo, daysHi
	if err := json.Unmarshal(weight, &rt.WeightTiers); err != nil {
		return rt, fmt.Errorf("decode weight tiers: %w", err)
	}
	if err := json.Unmarshal(price, &rt.PriceTiers); err != nil {
		return rt, fmt.Errorf("decode price tiers: %w", err)
	}
	return rt, nil
}

// Rates returns the shop's rates, of one zone when zoneID is set.
func (Shipping) Rates(ctx context.Context, tx pgx.Tx, tenantID, zoneID string) ([]domain.Rate, error) {
	q := `SELECT ` + rateColumns + ` FROM rates WHERE tenant_id = $1`
	args := []any{tenantID}
	if zoneID != "" {
		q += ` AND zone_id = $2`
		args = append(args, zoneID)
	}
	rows, err := tx.Query(ctx, q+` ORDER BY zone_id, method`, args...)
	if err != nil {
		return nil, fmt.Errorf("list rates: %w", err)
	}
	rates, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Rate, error) { return scanRate(r) })
	if err != nil {
		return nil, fmt.Errorf("list rates: %w", err)
	}
	return rates, nil
}

// Rate returns one rate, or false.
func (Shipping) Rate(ctx context.Context, tx pgx.Tx, tenantID, id string) (domain.Rate, bool, error) {
	rt, err := scanRate(tx.QueryRow(ctx, `SELECT `+rateColumns+` FROM rates WHERE tenant_id = $1 AND id = $2`, tenantID, id))
	if dbkit.IsNoRows(err) {
		return rt, false, nil
	}
	if err != nil {
		return rt, false, fmt.Errorf("get rate: %w", err)
	}
	return rt, true, nil
}

// PutRate creates or replaces a rate.
func (Shipping) PutRate(ctx context.Context, tx pgx.Tx, tenantID string, r domain.Rate) error {
	weight, err := json.Marshal(nonNil(r.WeightTiers))
	if err != nil {
		return fmt.Errorf("encode weight tiers: %w", err)
	}
	price, err := json.Marshal(nonNil(r.PriceTiers))
	if err != nil {
		return fmt.Errorf("encode price tiers: %w", err)
	}
	status := "inactive"
	if r.Active {
		status = "active"
	}
	_, err = tx.Exec(ctx, `INSERT INTO rates (id, tenant_id, zone_id, method, display_name, pricing_type,
			flat_amount_minor, weight_tiers, price_tiers, estimated_days_min, estimated_days_max, carrier_driver,
			schedule_id, requires_delivery_slot, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, NULLIF($13, '')::uuid, $14, $15, $16, $17)
		ON CONFLICT (id) DO UPDATE SET zone_id = EXCLUDED.zone_id, method = EXCLUDED.method,
			display_name = EXCLUDED.display_name, pricing_type = EXCLUDED.pricing_type,
			flat_amount_minor = EXCLUDED.flat_amount_minor, weight_tiers = EXCLUDED.weight_tiers,
			price_tiers = EXCLUDED.price_tiers, estimated_days_min = EXCLUDED.estimated_days_min,
			estimated_days_max = EXCLUDED.estimated_days_max, carrier_driver = EXCLUDED.carrier_driver,
			schedule_id = EXCLUDED.schedule_id, requires_delivery_slot = EXCLUDED.requires_delivery_slot,
			status = EXCLUDED.status, updated_at = EXCLUDED.updated_at`,
		r.ID, tenantID, r.ZoneID, r.Method, r.DisplayName, string(r.PricingType), r.FlatAmountMinor, weight, price,
		r.EstimatedDaysMin, r.EstimatedDaysMax, r.CarrierDriver, r.ScheduleID, r.RequiresDeliverySlot, status,
		r.CreatedAt, r.UpdatedAt)
	if err != nil {
		return fmt.Errorf("put rate: %w", err)
	}
	return nil
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// DeleteRate deletes a rate, reporting whether it existed.
func (Shipping) DeleteRate(ctx context.Context, tx pgx.Tx, tenantID, id string) (bool, error) {
	tag, err := tx.Exec(ctx, `DELETE FROM rates WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return false, fmt.Errorf("delete rate: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

const shipmentColumns = `id, order_id, order_number, COALESCE(rate_id::text, ''), method, carrier_driver,
	tracking_number, COALESCE(label_url, ''), status, delivery_address, COALESCE(carrier_name, ''),
	COALESCE(carrier_reference, ''), created_at, updated_at`

func scanShipment(r pgx.Row) (domain.Shipment, error) {
	var (
		s      domain.Shipment
		status string
		addr   []byte
	)
	err := r.Scan(&s.ID, &s.OrderID, &s.OrderNumber, &s.RateID, &s.Method, &s.CarrierDriver, &s.TrackingNumber,
		&s.LabelURL, &status, &addr, &s.CarrierName, &s.CarrierReference, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return s, err
	}
	s.Status = domain.Status(status)
	if err := json.Unmarshal(addr, &s.DeliveryAddress); err != nil {
		return s, fmt.Errorf("decode delivery address: %w", err)
	}
	return s, nil
}

// Shipment returns a shipment by ID, locked when forUpdate, or false.
func (Shipping) Shipment(ctx context.Context, tx pgx.Tx, tenantID, id string, forUpdate bool) (domain.Shipment, bool, error) {
	return shipmentWhere(ctx, tx, `tenant_id = $1 AND id = $2`, forUpdate, tenantID, id)
}

// ShipmentByOrder returns an order's shipment, locked when forUpdate, or
// false.
func (Shipping) ShipmentByOrder(ctx context.Context, tx pgx.Tx, tenantID, orderID string, forUpdate bool) (domain.Shipment, bool, error) {
	return shipmentWhere(ctx, tx, `tenant_id = $1 AND order_id = $2`, forUpdate, tenantID, orderID)
}

// ShipmentByTracking returns the shipment with a tracking number, or false.
func (Shipping) ShipmentByTracking(ctx context.Context, tx pgx.Tx, tenantID, tracking string) (domain.Shipment, bool, error) {
	return shipmentWhere(ctx, tx, `tenant_id = $1 AND tracking_number = $2`, false, tenantID, tracking)
}

func shipmentWhere(ctx context.Context, tx pgx.Tx, where string, forUpdate bool, args ...any) (domain.Shipment, bool, error) {
	q := `SELECT ` + shipmentColumns + ` FROM shipments WHERE ` + where
	if forUpdate {
		q += ` FOR UPDATE`
	}
	s, err := scanShipment(tx.QueryRow(ctx, q, args...))
	if dbkit.IsNoRows(err) {
		return s, false, nil
	}
	if err != nil {
		return s, false, fmt.Errorf("get shipment: %w", err)
	}
	return s, true, nil
}

// ShipmentCursor is the position after the last shipment of a page.
type ShipmentCursor struct {
	CreatedAt time.Time `json:"c"`
	ID        string    `json:"i"`
}

// ListShipments returns up to limit shipments, newest first, after the
// cursor, in status when set.
func (Shipping) ListShipments(ctx context.Context, tx pgx.Tx, tenantID string, status domain.Status, after *ShipmentCursor, limit int) ([]domain.Shipment, error) {
	q := `SELECT ` + shipmentColumns + ` FROM shipments WHERE tenant_id = $1`
	args := []any{tenantID}
	if status != "" {
		args = append(args, string(status))
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
		return nil, fmt.Errorf("list shipments: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Shipment, error) { return scanShipment(r) })
	if err != nil {
		return nil, fmt.Errorf("list shipments: %w", err)
	}
	return out, nil
}

// InsertShipment records a new shipment.
func (Shipping) InsertShipment(ctx context.Context, tx pgx.Tx, tenantID string, s domain.Shipment) error {
	addr, err := json.Marshal(s.DeliveryAddress)
	if err != nil {
		return fmt.Errorf("encode delivery address: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO shipments (id, tenant_id, order_id, order_number, rate_id, method,
			carrier_driver, tracking_number, label_url, status, delivery_address, created_at, updated_at)
		VALUES ($1, $2, $3, $4, NULLIF($5, '')::uuid, $6, $7, $8, NULLIF($9, ''), $10, $11, $12, $13)`,
		s.ID, tenantID, s.OrderID, s.OrderNumber, s.RateID, s.Method, s.CarrierDriver, s.TrackingNumber, s.LabelURL,
		string(s.Status), addr, s.CreatedAt, s.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert shipment: %w", err)
	}
	return nil
}

// UpdateShipment saves a shipment's status and carrier details.
func (Shipping) UpdateShipment(ctx context.Context, tx pgx.Tx, tenantID string, s domain.Shipment) error {
	_, err := tx.Exec(ctx, `UPDATE shipments SET status = $3, carrier_name = NULLIF($4, ''),
			carrier_reference = NULLIF($5, ''), updated_at = $6
		WHERE tenant_id = $1 AND id = $2`,
		tenantID, s.ID, string(s.Status), s.CarrierName, s.CarrierReference, s.UpdatedAt)
	if err != nil {
		return fmt.Errorf("update shipment: %w", err)
	}
	return nil
}

// ClearAddresses empties the delivery address of the orders' shipments
// (ADR-005), returning how many changed.
func (Shipping) ClearAddresses(ctx context.Context, tx pgx.Tx, tenantID string, orderIDs []string, now time.Time) (int64, error) {
	tag, err := tx.Exec(ctx, `UPDATE shipments SET delivery_address = '{}', updated_at = $3
		WHERE tenant_id = $1 AND order_id = ANY($2::uuid[]) AND delivery_address <> '{}'`, tenantID, orderIDs, now)
	if err != nil {
		return 0, fmt.Errorf("clear addresses: %w", err)
	}
	return tag.RowsAffected(), nil
}

// AddEvent records a step in a shipment's history.
func (Shipping) AddEvent(ctx context.Context, tx pgx.Tx, tenantID, shipmentID, actorID string, e domain.Event) error {
	_, err := tx.Exec(ctx, `INSERT INTO shipment_events (tenant_id, shipment_id, status, message, actor_id,
			occurred_at, recorded_at)
		VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), $6, $7)`,
		tenantID, shipmentID, string(e.Status), e.Message, actorID, e.OccurredAt, e.RecordedAt)
	if err != nil {
		return fmt.Errorf("add shipment event: %w", err)
	}
	return nil
}

// Events returns a shipment's history, oldest first.
func (Shipping) Events(ctx context.Context, tx pgx.Tx, tenantID, shipmentID string) ([]domain.Event, error) {
	rows, err := tx.Query(ctx, `SELECT status, COALESCE(message, ''), occurred_at, recorded_at
		FROM shipment_events WHERE tenant_id = $1 AND shipment_id = $2 ORDER BY occurred_at, recorded_at, id`,
		tenantID, shipmentID)
	if err != nil {
		return nil, fmt.Errorf("list shipment events: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Event, error) {
		var e domain.Event
		var status string
		err := r.Scan(&status, &e.Message, &e.OccurredAt, &e.RecordedAt)
		e.Status = domain.Status(status)
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("list shipment events: %w", err)
	}
	return out, nil
}
