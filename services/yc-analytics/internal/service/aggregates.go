package service

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	ycevents "github.com/slaghuis/yc-platform/libs/yc-events"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	"github.com/slaghuis/yc-platform/services/yc-analytics/internal/domain"
)

// Rebuild triggers (analytics.aggregate.rebuilt.v1).
const (
	TriggerSchedule = "schedule"
	TriggerManual   = "manual"
)

// Refresh rebuilds the aggregates of days whose facts changed and
// returns how many days it rebuilt.
func (s *Service) Refresh(ctx context.Context) (int, error) {
	var n int
	err := dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		days, err := s.aggs.ClaimDirty(ctx, tx, s.cfg.RefreshBatch)
		if err != nil {
			return err
		}
		for _, d := range days {
			if _, err := s.aggs.Rebuild(ctx, tx, d.TenantID, d.Date, d.Date); err != nil {
				return err
			}
		}
		n = len(days)
		return nil
	})
	return n, err
}

// RunRefresher refreshes the aggregates every interval until ctx ends.
func (s *Service) RunRefresher(ctx context.Context, interval time.Duration) {
	log := telemetry.LoggerFromContext(ctx)
	every(ctx, interval, func() {
		for {
			n, err := s.Refresh(ctx)
			if err != nil {
				log.Error("refresh aggregates failed", "error", err)
				return
			}
			if n < s.cfg.RefreshBatch {
				return
			}
		}
	})
}

// RebuildResult is what a rebuild did.
type RebuildResult struct {
	TenantID string
	Range    domain.Range
	Rows     int64
}

// Rebuild replaces the aggregates of a shop (or every shop, tenantID "")
// over r with rows computed from the facts (system admin).
func (s *Service) Rebuild(ctx context.Context, tenantID string, r domain.Range) (RebuildResult, error) {
	if err := requireSystemAdmin(ctx); err != nil {
		return RebuildResult{}, err
	}
	if tenantID == eventbus.PlatformTenantID {
		return RebuildResult{}, ycerrors.Validation().
			Field("tenant_id", "invalid", "analytics covers shops, not _platform").Build()
	}
	return s.rebuild(ctx, tenantID, r, TriggerManual)
}

func (s *Service) rebuild(ctx context.Context, tenantID string, r domain.Range, trigger string) (RebuildResult, error) {
	res := RebuildResult{TenantID: tenantID, Range: r}
	err := dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		if res.Rows, err = s.aggs.Rebuild(ctx, tx, tenantID, r.From, r.To); err != nil {
			return err
		}
		return s.publish(ctx, tx, eventbus.PlatformTenantID, ycevents.AnalyticsAggregateRebuiltV1{
			Aggregates: domain.Aggregates,
			From:       r.From.Format(domain.DateLayout),
			To:         r.To.Format(domain.DateLayout),
			Rows:       res.Rows,
			Trigger:    &trigger,
			RebuiltAt:  s.now().UTC(),
		})
	})
	return res, err
}

// Nightly rebuilds every shop's aggregates for the NightlyDays ending
// yesterday.
func (s *Service) Nightly(ctx context.Context) (RebuildResult, error) {
	to := domain.Date(s.now()).AddDate(0, 0, -1)
	return s.rebuild(ctx, "", domain.Range{From: to.AddDate(0, 0, 1-s.cfg.NightlyDays), To: to}, TriggerSchedule)
}

// RunNightly runs Nightly daily at the time of day at (UTC) until ctx ends.
func (s *Service) RunNightly(ctx context.Context, at time.Duration) {
	log := telemetry.LoggerFromContext(ctx)
	for {
		now := s.now().UTC()
		next := domain.Date(now).Add(at)
		if !next.After(now) {
			next = next.AddDate(0, 0, 1)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(next.Sub(now)):
		}
		res, err := s.Nightly(ctx)
		if err != nil {
			log.Error("nightly rebuild failed", "error", err)
			continue
		}
		log.Info("nightly rebuild done", "from", res.Range.From.Format(domain.DateLayout),
			"to", res.Range.To.Format(domain.DateLayout), "rows", res.Rows)
	}
}

// Consistency is the outcome of comparing aggregates with their facts.
type Consistency struct {
	Range      domain.Range
	Mismatches []domain.Mismatch
	// PendingDays are days whose facts changed and whose aggregates the
	// refresher has not rebuilt yet; they may show as mismatches.
	PendingDays int64
}

// CheckConsistency compares every aggregate with the facts over r, for a
// shop or every shop ("") (system admin).
func (s *Service) CheckConsistency(ctx context.Context, tenantID string, r domain.Range) (Consistency, error) {
	c := Consistency{Range: r}
	if err := requireSystemAdmin(ctx); err != nil {
		return c, err
	}
	err := dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		if c.Mismatches, err = s.aggs.Mismatches(ctx, tx, tenantID, r.From, r.To); err != nil {
			return err
		}
		c.PendingDays, err = s.aggs.Pending(ctx, tx, tenantID)
		return err
	})
	return c, err
}

// every runs fn every interval until ctx ends.
func every(ctx context.Context, interval time.Duration, fn func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		}
	}
}
