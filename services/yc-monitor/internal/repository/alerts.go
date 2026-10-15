package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/slaghuis/yc-platform/services/yc-monitor/internal/domain"
)

// Alerts stores alerts and threshold overrides.
type Alerts struct{}

// Thresholds returns the overrides for the rule in tenantID and _platform.
func (Alerts) Thresholds(ctx context.Context, tx pgx.Tx, tenantIDs []string, rule string) ([]domain.Thresholds, error) {
	rows, err := tx.Query(ctx, `
		SELECT tenant_id, rule, threshold, min_volume, window_minutes, enabled, updated_by, updated_at
		FROM alert_thresholds WHERE ($1::text[] IS NULL OR tenant_id = ANY($1)) AND ($2 = '' OR rule = $2)
		ORDER BY tenant_id, rule`, tenantIDs, rule)
	if err != nil {
		return nil, fmt.Errorf("list thresholds: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Thresholds, error) {
		var t domain.Thresholds
		var at time.Time
		err := r.Scan(&t.TenantID, &t.Rule, &t.Threshold, &t.MinVolume, &t.WindowMinutes, &t.Enabled, &t.UpdatedBy, &at)
		t.UpdatedAt = &at
		return t, err
	})
	if err != nil {
		return nil, fmt.Errorf("list thresholds: %w", err)
	}
	return out, nil
}

// PutThresholds creates or replaces an override.
func (Alerts) PutThresholds(ctx context.Context, tx pgx.Tx, t domain.Thresholds) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO alert_thresholds (tenant_id, rule, threshold, min_volume, window_minutes, enabled, updated_by, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (tenant_id, rule) DO UPDATE SET threshold = EXCLUDED.threshold, min_volume = EXCLUDED.min_volume,
			window_minutes = EXCLUDED.window_minutes, enabled = EXCLUDED.enabled,
			updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at`,
		t.TenantID, t.Rule, t.Threshold, t.MinVolume, t.WindowMinutes, t.Enabled, t.UpdatedBy, t.UpdatedAt)
	if err != nil {
		return fmt.Errorf("put thresholds: %w", err)
	}
	return nil
}

const alertColumns = `id, tenant_id, rule, metric, scope, dimensions, severity, summary, value, threshold, notify,
	occurrences, opened_at, last_seen_at, acknowledged_at, COALESCE(acknowledged_by, ''), resolved_at`

func scanAlert(row pgx.Row) (domain.Alert, error) {
	var a domain.Alert
	err := row.Scan(&a.ID, &a.TenantID, &a.Rule, &a.Metric, &a.Scope, &a.Dimensions, &a.Severity, &a.Summary,
		&a.Value, &a.Threshold, &a.Notify, &a.Occurrences, &a.OpenedAt, &a.LastSeenAt, &a.AcknowledgedAt,
		&a.AcknowledgedBy, &a.ResolvedAt)
	return a, err
}

func oneAlert(row pgx.Row) (domain.Alert, bool, error) {
	a, err := scanAlert(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, false, nil
	}
	if err != nil {
		return a, false, fmt.Errorf("get alert: %w", err)
	}
	return a, true, nil
}

// Active returns the unresolved alert for the rule and scope, locked.
func (Alerts) Active(ctx context.Context, tx pgx.Tx, tenantID, rule, scope string) (domain.Alert, bool, error) {
	return oneAlert(tx.QueryRow(ctx, `SELECT `+alertColumns+` FROM alerts
		WHERE tenant_id = $1 AND rule = $2 AND scope = $3 AND resolved_at IS NULL FOR UPDATE`, tenantID, rule, scope))
}

// ActiveByRule returns every unresolved alert of the rule.
func (Alerts) ActiveByRule(ctx context.Context, tx pgx.Tx, rule string) ([]domain.Alert, error) {
	rows, err := tx.Query(ctx, `SELECT `+alertColumns+` FROM alerts WHERE rule = $1 AND resolved_at IS NULL`, rule)
	if err != nil {
		return nil, fmt.Errorf("active alerts: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Alert, error) { return scanAlert(r) })
	if err != nil {
		return nil, fmt.Errorf("active alerts: %w", err)
	}
	return out, nil
}

// Get returns the alert, locked; an empty tenantID matches any tenant.
func (Alerts) Get(ctx context.Context, tx pgx.Tx, tenantID, id string) (domain.Alert, bool, error) {
	return oneAlert(tx.QueryRow(ctx, `SELECT `+alertColumns+` FROM alerts
		WHERE id = $1 AND ($2 = '' OR tenant_id = $2) FOR UPDATE`, id, tenantID))
}

// Insert adds a new alert.
func (Alerts) Insert(ctx context.Context, tx pgx.Tx, a domain.Alert) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO alerts (id, tenant_id, rule, metric, scope, dimensions, severity, summary, value, threshold,
			notify, occurrences, opened_at, last_seen_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		a.ID, a.TenantID, a.Rule, a.Metric, a.Scope, a.Dimensions, a.Severity, a.Summary, a.Value, a.Threshold,
		a.Notify, a.Occurrences, a.OpenedAt, a.LastSeenAt)
	if err != nil {
		return fmt.Errorf("insert alert: %w", err)
	}
	return nil
}

// Update writes an alert's mutable fields.
func (Alerts) Update(ctx context.Context, tx pgx.Tx, a domain.Alert) error {
	_, err := tx.Exec(ctx, `
		UPDATE alerts SET summary = $2, value = $3, threshold = $4, occurrences = $5, last_seen_at = $6,
			acknowledged_at = $7, acknowledged_by = NULLIF($8, ''), resolved_at = $9
		WHERE id = $1`,
		a.ID, a.Summary, a.Value, a.Threshold, a.Occurrences, a.LastSeenAt, a.AcknowledgedAt, a.AcknowledgedBy,
		a.ResolvedAt)
	if err != nil {
		return fmt.Errorf("update alert: %w", err)
	}
	return nil
}

// AlertCursor is the keyset position in an alert listing.
type AlertCursor struct {
	OpenedAt time.Time `json:"o"`
	ID       string    `json:"i"`
}

// List returns alerts newest first.
func (Alerts) List(ctx context.Context, tx pgx.Tx, f domain.AlertFilter, after *AlertCursor, limit int) ([]domain.Alert, error) {
	var afterAt *time.Time
	afterID := ""
	if after != nil {
		afterAt, afterID = &after.OpenedAt, after.ID
	}
	rows, err := tx.Query(ctx, `SELECT `+alertColumns+` FROM alerts
		WHERE ($1 = '' OR tenant_id = $1) AND ($2 = '' OR severity = $2) AND ($3 = '' OR rule = $3)
		  AND CASE $4
		        WHEN 'open' THEN resolved_at IS NULL AND acknowledged_at IS NULL
		        WHEN 'acknowledged' THEN resolved_at IS NULL AND acknowledged_at IS NOT NULL
		        WHEN 'resolved' THEN resolved_at IS NOT NULL
		        WHEN 'active' THEN resolved_at IS NULL
		        ELSE TRUE END
		  AND ($5::timestamptz IS NULL OR (opened_at, id) < ($5, $6::uuid))
		ORDER BY opened_at DESC, id DESC LIMIT $7`,
		f.TenantID, string(f.Severity), f.Rule, string(f.Status), afterAt, nullUUID(afterID), limit)
	if err != nil {
		return nil, fmt.Errorf("list alerts: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Alert, error) { return scanAlert(r) })
	if err != nil {
		return nil, fmt.Errorf("list alerts: %w", err)
	}
	return out, nil
}

// OpenCounts counts unresolved alerts by severity.
func (Alerts) OpenCounts(ctx context.Context, tx pgx.Tx, tenantID string) (map[domain.Severity]int, error) {
	rows, err := tx.Query(ctx, `SELECT severity, COUNT(*) FROM alerts
		WHERE resolved_at IS NULL AND ($1 = '' OR tenant_id = $1) GROUP BY severity`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("count alerts: %w", err)
	}
	defer rows.Close()
	out := map[domain.Severity]int{}
	for rows.Next() {
		var s domain.Severity
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, fmt.Errorf("count alerts: %w", err)
		}
		out[s] = n
	}
	return out, rows.Err()
}

func nullUUID(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
