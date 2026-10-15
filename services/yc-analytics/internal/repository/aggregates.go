package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/slaghuis/yc-platform/services/yc-analytics/internal/domain"
)

// Aggregates builds the aggregates from the facts.
type Aggregates struct{}

// builds are each aggregate's columns and the query computing its rows
// from the facts for tenant $1 (” for every tenant) and dates $2 to $3.
var builds = []struct {
	table, columns, query string
}{
	{
		domain.AggDailyRevenue,
		"tenant_id, date_key, currency, orders, cancelled, items_minor, tax_minor, shipping_minor, gross_minor, refunded_minor, net_minor",
		`SELECT tenant_id, date_key, currency,
			COUNT(*) FILTER (WHERE status = 'placed'),
			COUNT(*) FILTER (WHERE status = 'cancelled'),
			COALESCE(SUM(items_minor) FILTER (WHERE status = 'placed'), 0)::bigint,
			COALESCE(SUM(tax_minor) FILTER (WHERE status = 'placed'), 0)::bigint,
			COALESCE(SUM(shipping_minor) FILTER (WHERE status = 'placed'), 0)::bigint,
			COALESCE(SUM(total_minor) FILTER (WHERE status = 'placed'), 0)::bigint,
			COALESCE(SUM(refunded_minor) FILTER (WHERE status = 'placed'), 0)::bigint,
			COALESCE(SUM(total_minor - refunded_minor) FILTER (WHERE status = 'placed'), 0)::bigint
		FROM fact_order
		WHERE ($1 = '' OR tenant_id = $1) AND date_key BETWEEN $2 AND $3
		GROUP BY tenant_id, date_key, currency`,
	},
	{
		domain.AggDailyFunnel,
		"tenant_id, date_key, visitors, carts_created, checkouts_started, checkouts_confirmed, orders_placed",
		`SELECT tenant_id, date_key, SUM(v)::bigint, SUM(c)::bigint, SUM(s)::bigint, SUM(k)::bigint, SUM(o)::bigint
		FROM (
			SELECT tenant_id, date_key, 1 AS v, 0 AS c, 0 AS s, 0 AS k, 0 AS o FROM fact_visitor_session
			UNION ALL SELECT tenant_id, date_key, 0, 1, 0, 0, 0 FROM fact_cart_event WHERE event = 'created' AND in_funnel
			UNION ALL SELECT tenant_id, date_key, 0, 0, 1, 0, 0 FROM fact_checkout_step WHERE state = 'started'
			UNION ALL SELECT tenant_id, date_key, 0, 0, 0, 1, 0 FROM fact_checkout_step WHERE state = 'confirmed'
			UNION ALL SELECT tenant_id, date_key, 0, 0, 0, 0, 1 FROM fact_order
		) f
		WHERE ($1 = '' OR tenant_id = $1) AND date_key BETWEEN $2 AND $3
		GROUP BY tenant_id, date_key`,
	},
	{
		domain.AggProductPerformance,
		"tenant_id, date_key, sku_id, currency, name, units, revenue_minor",
		`SELECT i.tenant_id, i.date_key, i.sku_id, i.currency, MAX(i.name), SUM(i.quantity)::bigint,
			SUM(i.line_total_minor)::bigint
		FROM fact_order_item i JOIN fact_order o ON o.order_id = i.order_id AND o.status = 'placed'
		WHERE ($1 = '' OR i.tenant_id = $1) AND i.date_key BETWEEN $2 AND $3
		GROUP BY i.tenant_id, i.date_key, i.sku_id, i.currency`,
	},
	{
		domain.AggGatewayPerformance,
		"tenant_id, date_key, gateway, currency, attempts, successes, failures, amount_minor",
		`SELECT tenant_id, date_key, gateway, currency, COUNT(*),
			COUNT(*) FILTER (WHERE succeeded_at IS NOT NULL),
			COUNT(*) FILTER (WHERE failed_at IS NOT NULL AND succeeded_at IS NULL),
			COALESCE(SUM(amount_minor) FILTER (WHERE succeeded_at IS NOT NULL), 0)::bigint
		FROM fact_payment
		WHERE ($1 = '' OR tenant_id = $1) AND date_key BETWEEN $2 AND $3
		GROUP BY tenant_id, date_key, gateway, currency`,
	},
}

// Rebuild replaces the aggregates for tenantID (” for every tenant) over
// the dates from to to with rows computed from the facts, clears those
// days' dirty marks and returns the rows written.
func (Aggregates) Rebuild(ctx context.Context, tx pgx.Tx, tenantID string, from, to time.Time) (int64, error) {
	var rows int64
	for _, b := range builds {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE ($1 = '' OR tenant_id = $1) AND date_key BETWEEN $2 AND $3`, b.table),
			tenantID, from, to); err != nil {
			return rows, fmt.Errorf("clear %s: %w", b.table, err)
		}
		tag, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (%s) %s`, b.table, b.columns, b.query), tenantID, from, to)
		if err != nil {
			return rows, fmt.Errorf("build %s: %w", b.table, err)
		}
		rows += tag.RowsAffected()
	}
	if _, err := tx.Exec(ctx, `DELETE FROM agg_dirty WHERE ($1 = '' OR tenant_id = $1) AND date_key BETWEEN $2 AND $3`,
		tenantID, from, to); err != nil {
		return rows, fmt.Errorf("clear dirty days: %w", err)
	}
	return rows, nil
}

// MarkDirty records that a shop's day needs its aggregates rebuilt.
func (Aggregates) MarkDirty(ctx context.Context, tx pgx.Tx, tenantID string, dates []time.Time, now time.Time) error {
	if len(dates) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO agg_dirty (tenant_id, date_key, marked_at) SELECT $1, d, $3 FROM unnest($2::date[]) AS d
		ON CONFLICT (tenant_id, date_key) DO UPDATE SET marked_at = EXCLUDED.marked_at`,
		tenantID, dates, now)
	if err != nil {
		return fmt.Errorf("mark dirty: %w", err)
	}
	return nil
}

// ClaimDirty returns up to limit dirty days, locked until tx ends.
func (Aggregates) ClaimDirty(ctx context.Context, tx pgx.Tx, limit int) ([]domain.Day, error) {
	rows, err := tx.Query(ctx, `
		SELECT tenant_id, date_key FROM agg_dirty ORDER BY marked_at LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, fmt.Errorf("claim dirty days: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Day, error) {
		var d domain.Day
		err := r.Scan(&d.TenantID, &d.Date)
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("claim dirty days: %w", err)
	}
	return out, nil
}

// Pending counts dirty days, for tenantID or every tenant (”).
func (Aggregates) Pending(ctx context.Context, tx pgx.Tx, tenantID string) (int64, error) {
	var n int64
	err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM agg_dirty WHERE $1 = '' OR tenant_id = $1`, tenantID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count dirty days: %w", err)
	}
	return n, nil
}

// Mismatches compares each aggregate with its facts for tenantID (” for
// every tenant) over the dates from to to and returns the days that
// differ, including days that are missing on either side.
func (Aggregates) Mismatches(ctx context.Context, tx pgx.Tx, tenantID string, from, to time.Time) ([]domain.Mismatch, error) {
	var out []domain.Mismatch
	for _, b := range builds {
		rows, err := tx.Query(ctx, fmt.Sprintf(`
			WITH expected AS (%[3]s),
			actual AS (SELECT %[2]s FROM %[1]s WHERE ($1 = '' OR tenant_id = $1) AND date_key BETWEEN $2 AND $3)
			SELECT DISTINCT tenant_id, date_key FROM (
				(SELECT * FROM expected EXCEPT SELECT * FROM actual)
				UNION ALL
				(SELECT * FROM actual EXCEPT SELECT * FROM expected)
			) d ORDER BY date_key, tenant_id`, b.table, b.columns, b.query), tenantID, from, to)
		if err != nil {
			return nil, fmt.Errorf("compare %s: %w", b.table, err)
		}
		days, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Mismatch, error) {
			m := domain.Mismatch{Aggregate: b.table}
			err := r.Scan(&m.TenantID, &m.Date)
			return m, err
		})
		if err != nil {
			return nil, fmt.Errorf("compare %s: %w", b.table, err)
		}
		out = append(out, days...)
	}
	return out, nil
}
