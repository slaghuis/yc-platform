package service

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	"github.com/slaghuis/yc-platform/services/yc-monitor/internal/domain"
)

// maxPoints caps a metric series.
const maxPoints = 1440

// Count adds an event to a counter in the event's tenant, once per event.
func (s *Service) Count(ctx context.Context, env eventbus.Envelope, metric string, dims map[string]string) error {
	return tenantTx(ctx, s.db, env.TenantID, func(tx pgx.Tx) error {
		fresh, err := s.metrics.Counted(ctx, tx, env.TenantID, env.EventID, s.now().UTC())
		if err != nil || !fresh {
			return err
		}
		return s.metrics.Increment(ctx, tx, domain.Count{
			TenantID: env.TenantID, Metric: metric, Dimensions: dims, At: env.EventTime,
		})
	})
}

// Series returns a counter per bucket: summed over every shop for a system
// admin (unless q names one), the caller's shop for a shop admin. From
// defaults to 60 buckets before To, which defaults to now.
func (s *Service) Series(ctx context.Context, q domain.MetricQuery) ([]domain.Point, error) {
	v := ycerrors.Validation()
	if !domain.ValidMetric(q.Metric) {
		v.Field("metric", "invalid", "Unknown metric")
	}
	if q.Bucket == "" {
		q.Bucket = domain.Bucket1h
	}
	if _, err := domain.ParseBucket(string(q.Bucket)); err != nil {
		return nil, v.Field("bucket", "invalid", "Bucket is 1m, 5m, 1h or 1d").Build()
	}
	if q.To.IsZero() {
		q.To = s.now().UTC()
	}
	if q.From.IsZero() {
		q.From = q.Bucket.Start(q.To).Add(-59 * q.Bucket.Duration())
	}
	if !q.From.Before(q.To) {
		v.Field("from", "invalid", "From must be before to")
	} else if q.To.Sub(q.From)/q.Bucket.Duration() > maxPoints {
		v.Field("from", "out_of_range", "At most 1440 buckets; use a larger bucket")
	}
	if v.HasErrors() {
		return nil, v.Build()
	}
	var out []domain.Point
	err := s.readScoped(ctx, func(tx pgx.Tx, tenantID string) error {
		if tenantID != "" {
			q.TenantID = tenantID
		}
		var err error
		out, err = s.metrics.Series(ctx, tx, q)
		return err
	})
	return out, err
}

// Dashboard is the platform at a glance.
type Dashboard struct {
	// Status is red with an unhealthy service or open critical alert,
	// amber with an open warning or a service not yet checked, else green.
	Status                    string
	Services                  map[domain.HealthStatus]int
	OpenAlerts                map[domain.Severity]int
	DeadLettersUnacknowledged int64
	// LastHour counts each metric over the last 60 one-minute buckets.
	LastHour    map[string]int64
	GeneratedAt time.Time
}

// Dashboard summarises health, alerts and the last hour (system admin).
func (s *Service) Dashboard(ctx context.Context) (Dashboard, error) {
	d := Dashboard{
		Services: map[domain.HealthStatus]int{}, LastHour: map[string]int64{}, GeneratedAt: s.now().UTC(),
	}
	if err := requireSystemAdmin(ctx); err != nil {
		return d, err
	}
	err := dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		services, err := s.services.List(ctx, tx)
		if err != nil {
			return err
		}
		for _, svc := range services {
			d.Services[svc.Status]++
		}
		if d.OpenAlerts, err = s.alerts.OpenCounts(ctx, tx, ""); err != nil {
			return err
		}
		summary, err := s.dlq.Summary(ctx, tx, "")
		if err != nil {
			return err
		}
		for _, row := range summary {
			d.DeadLettersUnacknowledged += row.Unacknowledged
		}
		totals, err := s.metrics.Totals(ctx, tx, domain.Metrics, domain.Bucket1m.Start(d.GeneratedAt).Add(-59*time.Minute))
		if err != nil {
			return err
		}
		for _, m := range domain.Metrics {
			d.LastHour[m] = 0
		}
		for _, byMetric := range totals {
			for m, n := range byMetric {
				d.LastHour[m] += n
			}
		}
		return nil
	})
	switch {
	case d.Services[domain.HealthUnhealthy] > 0 || d.OpenAlerts[domain.SeverityCritical] > 0:
		d.Status = "red"
	case d.Services[domain.HealthUnknown] > 0 || d.OpenAlerts[domain.SeverityWarning] > 0:
		d.Status = "amber"
	default:
		d.Status = "green"
	}
	return d, err
}

// Purge deletes rollups, dead letters and received events past their
// retention.
func (s *Service) Purge(ctx context.Context) error {
	now := s.now().UTC()
	return dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := s.metrics.Purge(ctx, tx, s.cfg.Retention, 7*24*time.Hour, now); err != nil {
			return err
		}
		_, err := s.dlq.Purge(ctx, tx, now.Add(-s.cfg.DLQRetention), now.Add(-s.cfg.EventRetention))
		return err
	})
}

// RunPurger purges every interval until ctx ends.
func (s *Service) RunPurger(ctx context.Context, interval time.Duration) {
	every(ctx, interval, "retention purge", s.Purge)
}
