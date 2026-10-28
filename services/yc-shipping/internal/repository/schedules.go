package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	"github.com/slaghuis/yc-platform/services/yc-shipping/internal/domain"
)

const scheduleColumns = `id, name, timezone, window_minutes, weekdays, open_time::text, close_time::text,
	capacity_per_window, min_lead_minutes, same_day_cutoff::text, blackout_dates, active, created_at, updated_at`

func scanSchedule(r pgx.Row) (domain.Schedule, error) {
	var (
		s         domain.Schedule
		weekdays  []int32
		openRaw   string
		closeRaw  string
		cutoffRaw *string
		blackouts []time.Time
	)
	err := r.Scan(&s.ID, &s.Name, &s.Timezone, &s.WindowMinutes, &weekdays, &openRaw, &closeRaw,
		&s.CapacityPerWindow, &s.MinLeadMinutes, &cutoffRaw, &blackouts, &s.Active, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return s, err
	}
	s.OpenTime = clockFromPG(openRaw)
	s.CloseTime = clockFromPG(closeRaw)
	if cutoffRaw != nil {
		c := clockFromPG(*cutoffRaw)
		s.SameDayCutoff = &c
	}
	s.Weekdays = make([]int, len(weekdays))
	for i, d := range weekdays {
		s.Weekdays[i] = int(d)
	}
	s.BlackoutDates = make([]string, len(blackouts))
	for i, d := range blackouts {
		s.BlackoutDates[i] = d.Format("2006-01-02")
	}
	return s, nil
}

// clockFromPG trims Postgres TIME text (HH:MM:SS[.ffffff]) to HH:MM.
func clockFromPG(raw string) string {
	if len(raw) >= 5 {
		return raw[:5]
	}
	return raw
}

// Schedules returns the shop's delivery schedules by name.
func (Shipping) Schedules(ctx context.Context, tx pgx.Tx, tenantID string) ([]domain.Schedule, error) {
	rows, err := tx.Query(ctx, `SELECT `+scheduleColumns+` FROM delivery_schedules WHERE tenant_id = $1
		ORDER BY name, id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list schedules: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Schedule, error) { return scanSchedule(r) })
	if err != nil {
		return nil, fmt.Errorf("list schedules: %w", err)
	}
	return out, nil
}

// Schedule returns one schedule, or false.
func (Shipping) Schedule(ctx context.Context, tx pgx.Tx, tenantID, id string) (domain.Schedule, bool, error) {
	s, err := scanSchedule(tx.QueryRow(ctx, `SELECT `+scheduleColumns+` FROM delivery_schedules
		WHERE tenant_id = $1 AND id = $2`, tenantID, id))
	if dbkit.IsNoRows(err) {
		return s, false, nil
	}
	if err != nil {
		return s, false, fmt.Errorf("get schedule: %w", err)
	}
	return s, true, nil
}

// PutSchedule creates or replaces a delivery schedule.
func (Shipping) PutSchedule(ctx context.Context, tx pgx.Tx, tenantID string, s domain.Schedule) error {
	weekdays := make([]int32, len(s.Weekdays))
	for i, d := range s.Weekdays {
		if d < 0 || d > 6 {
			return fmt.Errorf("weekday out of range: %d", d)
		}
		weekdays[i] = int32(d) // domain validates 0–6; bounds checked above
	}
	var cutoff any
	if s.SameDayCutoff != nil {
		cutoff = *s.SameDayCutoff
	}
	blackouts := make([]time.Time, len(s.BlackoutDates))
	for i, d := range s.BlackoutDates {
		t, err := time.Parse("2006-01-02", d)
		if err != nil {
			return fmt.Errorf("encode blackout date: %w", err)
		}
		blackouts[i] = t
	}
	_, err := tx.Exec(ctx, `INSERT INTO delivery_schedules (id, tenant_id, name, timezone, window_minutes, weekdays,
			open_time, close_time, capacity_per_window, min_lead_minutes, same_day_cutoff, blackout_dates, active,
			created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7::time, $8::time, $9, $10, $11::time, $12, $13, $14, $15)
		ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, timezone = EXCLUDED.timezone,
			window_minutes = EXCLUDED.window_minutes, weekdays = EXCLUDED.weekdays, open_time = EXCLUDED.open_time,
			close_time = EXCLUDED.close_time, capacity_per_window = EXCLUDED.capacity_per_window,
			min_lead_minutes = EXCLUDED.min_lead_minutes, same_day_cutoff = EXCLUDED.same_day_cutoff,
			blackout_dates = EXCLUDED.blackout_dates, active = EXCLUDED.active, updated_at = EXCLUDED.updated_at`,
		s.ID, tenantID, s.Name, s.Timezone, s.WindowMinutes, weekdays, s.OpenTime, s.CloseTime,
		s.CapacityPerWindow, s.MinLeadMinutes, cutoff, blackouts, s.Active, s.CreatedAt, s.UpdatedAt)
	if err != nil {
		return fmt.Errorf("put schedule: %w", err)
	}
	return nil
}

// DeleteSchedule deletes a schedule when no rates reference it.
func (Shipping) DeleteSchedule(ctx context.Context, tx pgx.Tx, tenantID, id string) (bool, error) {
	tag, err := tx.Exec(ctx, `DELETE FROM delivery_schedules WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return false, fmt.Errorf("delete schedule: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// RatesReferencingSchedule reports whether any rate still uses the schedule.
func (Shipping) RatesReferencingSchedule(ctx context.Context, tx pgx.Tx, tenantID, scheduleID string) (bool, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM rates WHERE tenant_id = $1 AND schedule_id = $2`,
		tenantID, scheduleID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("count rates for schedule: %w", err)
	}
	return n > 0, nil
}

// BookingCount returns how many bookings hold a given window start.
func (Shipping) BookingCount(ctx context.Context, tx pgx.Tx, tenantID, scheduleID string, start time.Time) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM slot_bookings
		WHERE tenant_id = $1 AND schedule_id = $2 AND slot_start = $3`,
		tenantID, scheduleID, start.UTC()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count bookings: %w", err)
	}
	return n, nil
}

// BookingCounts returns booking counts keyed by Unix start for the schedule.
func (Shipping) BookingCounts(ctx context.Context, tx pgx.Tx, tenantID, scheduleID string, starts []time.Time) (map[int64]int, error) {
	out := make(map[int64]int, len(starts))
	if len(starts) == 0 {
		return out, nil
	}
	args := make([]time.Time, len(starts))
	for i, t := range starts {
		args[i] = t.UTC()
	}
	rows, err := tx.Query(ctx, `SELECT slot_start, COUNT(*)::int FROM slot_bookings
		WHERE tenant_id = $1 AND schedule_id = $2 AND slot_start = ANY($3::timestamptz[])
		GROUP BY slot_start`, tenantID, scheduleID, args)
	if err != nil {
		return nil, fmt.Errorf("count bookings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var start time.Time
		var n int
		if err := rows.Scan(&start, &n); err != nil {
			return nil, fmt.Errorf("count bookings: %w", err)
		}
		out[start.UTC().Unix()] = n
	}
	return out, rows.Err()
}

// SlotBooking is one capacity hold.
type SlotBooking struct {
	ID         string
	ScheduleID string
	SlotStart  time.Time
	SlotEnd    time.Time
	BookingKey string
	CreatedAt  time.Time
}

// BookingByKey returns the booking for a checkout key, or false.
func (Shipping) BookingByKey(ctx context.Context, tx pgx.Tx, tenantID, bookingKey string) (SlotBooking, bool, error) {
	var b SlotBooking
	err := tx.QueryRow(ctx, `SELECT id, schedule_id, slot_start, slot_end, booking_key, created_at
		FROM slot_bookings WHERE tenant_id = $1 AND booking_key = $2
		ORDER BY created_at DESC LIMIT 1`, tenantID, bookingKey).Scan(
		&b.ID, &b.ScheduleID, &b.SlotStart, &b.SlotEnd, &b.BookingKey, &b.CreatedAt)
	if dbkit.IsNoRows(err) {
		return b, false, nil
	}
	if err != nil {
		return b, false, fmt.Errorf("get booking by key: %w", err)
	}
	return b, true, nil
}

// BookSlot inserts a booking when capacity remains. Ok is false when full.
// Idempotent: an existing row for the same key and start returns ok without insert.
// Locks the schedule row so concurrent books of the last capacity serialise.
func (Shipping) BookSlot(ctx context.Context, tx pgx.Tx, tenantID string, b SlotBooking, capacity int) (bool, error) {
	var existing int
	err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM slot_bookings
		WHERE tenant_id = $1 AND schedule_id = $2 AND slot_start = $3 AND booking_key = $4`,
		tenantID, b.ScheduleID, b.SlotStart.UTC(), b.BookingKey).Scan(&existing)
	if err != nil {
		return false, fmt.Errorf("check booking: %w", err)
	}
	if existing > 0 {
		return true, nil
	}
	var sid string
	err = tx.QueryRow(ctx, `SELECT id FROM delivery_schedules WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
		tenantID, b.ScheduleID).Scan(&sid)
	if err != nil {
		return false, fmt.Errorf("lock schedule: %w", err)
	}
	var n int
	err = tx.QueryRow(ctx, `SELECT COUNT(*) FROM slot_bookings
		WHERE tenant_id = $1 AND schedule_id = $2 AND slot_start = $3`,
		tenantID, b.ScheduleID, b.SlotStart.UTC()).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("count bookings: %w", err)
	}
	if n >= capacity {
		return false, nil
	}
	if b.ID == "" {
		b.ID = uuid.NewString()
	}
	_, err = tx.Exec(ctx, `INSERT INTO slot_bookings (id, tenant_id, schedule_id, slot_start, slot_end, booking_key, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		b.ID, tenantID, b.ScheduleID, b.SlotStart.UTC(), b.SlotEnd.UTC(), b.BookingKey, b.CreatedAt.UTC())
	if dbkit.IsUniqueViolation(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("book slot: %w", err)
	}
	return true, nil
}

// ReleaseByKey deletes bookings for the key, optionally limited to one start.
func (Shipping) ReleaseByKey(ctx context.Context, tx pgx.Tx, tenantID, bookingKey string, start *time.Time) (bool, error) {
	q := `DELETE FROM slot_bookings WHERE tenant_id = $1 AND booking_key = $2`
	args := []any{tenantID, bookingKey}
	if start != nil {
		q += ` AND slot_start = $3`
		args = append(args, start.UTC())
	}
	tag, err := tx.Exec(ctx, q, args...)
	if err != nil {
		return false, fmt.Errorf("release slot: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
