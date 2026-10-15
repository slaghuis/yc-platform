package service

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	"github.com/slaghuis/yc-platform/services/yc-pricing/internal/integration"
	"github.com/slaghuis/yc-platform/services/yc-pricing/internal/repository"
)

// RefreshConfig is the `rates:` block's schedule.
type RefreshConfig struct {
	// Interval between refreshes (1h: ECB rates change daily).
	Interval time.Duration `koanf:"refresh_interval"`
	// StaleAfter is how long without a successful fetch before
	// pricing.exchange_rates_stale.v1 is published, at most once per period.
	StaleAfter time.Duration `koanf:"stale_after"`
	// BaseCurrencies are always refreshed, besides every store currency.
	BaseCurrencies []string `koanf:"base_currencies"`
}

// Rates keeps exchange rates fresh for display conversion (ADR-014).
type Rates struct {
	db       *pgxpool.Pool
	bus      Publisher
	repo     repository.Pricing
	provider integration.RateProvider
	cfg      RefreshConfig
	now      func() time.Time
}

// NewRates returns a refresher.
func NewRates(db *pgxpool.Pool, bus Publisher, provider integration.RateProvider, cfg RefreshConfig) *Rates {
	return &Rates{db: db, bus: bus, provider: provider, cfg: cfg, now: time.Now}
}

// Run refreshes now and then every interval until ctx is done.
func (r *Rates) Run(ctx context.Context) {
	logger := telemetry.LoggerFromContext(ctx)
	t := time.NewTicker(r.cfg.Interval)
	defer t.Stop()
	for {
		if err := r.RefreshAll(ctx); err != nil && ctx.Err() == nil {
			logger.Warn("exchange rate refresh failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RefreshAll refreshes the configured base currencies and every store
// currency.
func (r *Rates) RefreshAll(ctx context.Context) error {
	bases := slices.Clone(r.cfg.BaseCurrencies)
	err := dbkit.WithPlatformTx(ctx, r.db, func(tx pgx.Tx) error {
		stores, err := r.repo.StoreCurrencies(ctx, tx)
		bases = append(bases, stores...)
		return err
	})
	if err != nil {
		return err
	}
	slices.Sort(bases)
	var errs []error
	for _, base := range slices.Compact(bases) {
		errs = append(errs, r.Refresh(ctx, base))
	}
	return errors.Join(errs...)
}

// EnsureFresh refreshes base unless it was fetched successfully within the
// refresh interval, so a new store currency need not wait for the next
// scheduled refresh. A failed fetch is only logged: the schedule retries it.
func (r *Rates) EnsureFresh(ctx context.Context, base string) error {
	var status *repository.FetchStatus
	err := dbkit.InTx(ctx, r.db, func(tx pgx.Tx) error {
		var err error
		status, err = r.repo.FetchStatus(ctx, tx, base)
		return err
	})
	if err != nil {
		return err
	}
	if status != nil && status.LastSuccessAt != nil && r.now().Sub(*status.LastSuccessAt) < r.cfg.Interval {
		return nil
	}
	if err := r.Refresh(ctx, base); err != nil {
		telemetry.LoggerFromContext(ctx).Warn("exchange rate refresh failed", "base", base, "err", err)
	}
	return nil
}

// Refresh fetches base's rates and stores them. On failure it records the
// error and, once the rates are older than StaleAfter, reports them stale.
func (r *Rates) Refresh(ctx context.Context, base string) error {
	rates, fetchErr := r.provider.Latest(ctx, base)
	now := r.now()
	provider := r.provider.Name()
	err := dbkit.InTx(ctx, r.db, func(tx pgx.Tx) error {
		if fetchErr == nil {
			if err := r.repo.SaveRates(ctx, tx, base, provider, rates, now); err != nil {
				return err
			}
			if err := r.repo.RecordFetch(ctx, tx, base, provider, now, ""); err != nil {
				return err
			}
			return publish(ctx, r.bus, tx, eventbus.PlatformTenantID, events.TypePricingExchangeRatesUpdatedV1,
				events.PricingExchangeRatesUpdatedV1{
					BaseCurrency: base, Provider: provider, RateCount: int64(len(rates)), FetchedAt: now.UTC(),
				})
		}

		status, err := r.repo.FetchStatus(ctx, tx, base)
		if err != nil {
			return err
		}
		if err := r.repo.RecordFetch(ctx, tx, base, provider, now, fetchErr.Error()); err != nil {
			return err
		}
		if status == nil || status.LastSuccessAt == nil {
			return nil
		}
		age := now.Sub(*status.LastSuccessAt)
		reported := status.StaleReportedAt != nil && now.Sub(*status.StaleReportedAt) < r.cfg.StaleAfter
		if age < r.cfg.StaleAfter || reported {
			return nil
		}
		if err := r.repo.MarkStaleReported(ctx, tx, base, now); err != nil {
			return err
		}
		return publish(ctx, r.bus, tx, eventbus.PlatformTenantID, events.TypePricingExchangeRatesStaleV1,
			events.PricingExchangeRatesStaleV1{
				BaseCurrency: base, Provider: provider, LastFetchedAt: status.LastSuccessAt.UTC(),
				StaleForSeconds: int64(age.Seconds()),
			})
	})
	return errors.Join(fetchErr, err)
}
