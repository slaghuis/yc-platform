package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/slaghuis/yc-platform/services/yc-monitor/internal/domain"
)

// Metrics stores counter rollups.
type Metrics struct{}

// Counted records that eventID was counted and reports whether it is new.
func (Metrics) Counted(ctx context.Context, tx pgx.Tx, tenantID, eventID string, now time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, `INSERT INTO counted_events (tenant_id, event_id, counted_at) VALUES ($1, $2, $3)
		ON CONFLICT (event_id) DO NOTHING`, tenantID, eventID, now)
	if err != nil {
		return false, fmt.Errorf("record counted event: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Increment adds one to the counter in every bucket size.
func (Metrics) Increment(ctx context.Context, tx pgx.Tx, c domain.Count) error {
	sizes := make([]string, len(domain.Buckets))
	starts := make([]time.Time, len(domain.Buckets))
	for i, b := range domain.Buckets {
		sizes[i], starts[i] = string(b), b.Start(c.At)
	}
	dims := c.Dimensions
	if dims == nil {
		dims = map[string]string{}
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO metric_rollups (tenant_id, metric, bucket_size, bucket_start, dimensions, value)
		SELECT $1, $2, u.size, u.start, $5, 1 FROM unnest($3::text[], $4::timestamptz[]) AS u(size, start)
		ON CONFLICT (tenant_id, metric, bucket_size, bucket_start, dimensions)
		DO UPDATE SET value = metric_rollups.value + 1`,
		c.TenantID, c.Metric, sizes, starts, dims)
	if err != nil {
		return fmt.Errorf("increment %s: %w", c.Metric, err)
	}
	return nil
}

// Totals sums the 1m buckets from since on, per tenant and metric.
func (Metrics) Totals(ctx context.Context, tx pgx.Tx, metrics []string, since time.Time) (map[string]map[string]int64, error) {
	rows, err := tx.Query(ctx, `
		SELECT tenant_id, metric, SUM(value)::bigint FROM metric_rollups
		WHERE metric = ANY($1) AND bucket_size = '1m' AND bucket_start >= $2
		GROUP BY tenant_id, metric`, metrics, since)
	if err != nil {
		return nil, fmt.Errorf("sum metrics: %w", err)
	}
	defer rows.Close()
	out := map[string]map[string]int64{}
	for rows.Next() {
		var tenant, metric string
		var n int64
		if err := rows.Scan(&tenant, &metric, &n); err != nil {
			return nil, fmt.Errorf("sum metrics: %w", err)
		}
		if out[tenant] == nil {
			out[tenant] = map[string]int64{}
		}
		out[tenant][metric] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sum metrics: %w", err)
	}
	return out, nil
}

// Series returns the counter per bucket, summed across matching rows.
func (Metrics) Series(ctx context.Context, tx pgx.Tx, q domain.MetricQuery) ([]domain.Point, error) {
	dims := q.Dimensions
	if dims == nil {
		dims = map[string]string{}
	}
	rows, err := tx.Query(ctx, `
		SELECT bucket_start, SUM(value)::bigint FROM metric_rollups
		WHERE metric = $1 AND bucket_size = $2 AND bucket_start >= $3 AND bucket_start < $4
		  AND ($5 = '' OR tenant_id = $5) AND dimensions @> $6
		GROUP BY bucket_start ORDER BY bucket_start`,
		q.Metric, string(q.Bucket), q.From, q.To, q.TenantID, dims)
	if err != nil {
		return nil, fmt.Errorf("metric series: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Point, error) {
		var p domain.Point
		err := r.Scan(&p.BucketStart, &p.Value)
		return p, err
	})
	if err != nil {
		return nil, fmt.Errorf("metric series: %w", err)
	}
	return out, nil
}

// Retention is how long each bucket size is kept.
type Retention map[domain.Bucket]time.Duration

// Purge deletes rollups past their retention and counted events older
// than countedFor.
func (Metrics) Purge(ctx context.Context, tx pgx.Tx, keep Retention, countedFor time.Duration, now time.Time) (int64, error) {
	var n int64
	for size, d := range keep {
		tag, err := tx.Exec(ctx, `DELETE FROM metric_rollups WHERE bucket_size = $1 AND bucket_start < $2`,
			string(size), now.Add(-d))
		if err != nil {
			return n, fmt.Errorf("purge %s rollups: %w", size, err)
		}
		n += tag.RowsAffected()
	}
	tag, err := tx.Exec(ctx, `DELETE FROM counted_events WHERE counted_at < $1`, now.Add(-countedFor))
	if err != nil {
		return n, fmt.Errorf("purge counted events: %w", err)
	}
	return n + tag.RowsAffected(), nil
}
