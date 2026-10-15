package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	money "github.com/slaghuis/yc-platform/libs/yc-money"
	"github.com/slaghuis/yc-platform/services/yc-pricing/internal/domain"
)

// FetchStatus is the refresh state of one base currency's rates.
type FetchStatus struct {
	Base            string
	Provider        string
	LastSuccessAt   *time.Time
	LastAttemptAt   time.Time
	LastError       string
	StaleReportedAt *time.Time
}

// SaveRates stores the latest rates from base.
func (Pricing) SaveRates(ctx context.Context, tx pgx.Tx, base, provider string, rates map[string]money.Rate, at time.Time) error {
	batch := &pgx.Batch{}
	for quote, r := range rates {
		batch.Queue(`
			INSERT INTO exchange_rates (base_currency, quote_currency, rate, provider, fetched_at)
			VALUES ($1, $2, $3::numeric, $4, $5)
			ON CONFLICT (base_currency, quote_currency) DO UPDATE SET rate = EXCLUDED.rate,
				provider = EXCLUDED.provider, fetched_at = EXCLUDED.fetched_at`,
			base, quote, r.String(), provider, at)
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("save rates: %w", err)
	}
	return nil
}

// Rate returns the latest rate from base to quote, inverting the reverse
// rate when only that is known, or nil.
func (Pricing) Rate(ctx context.Context, tx pgx.Tx, base, quote string) (*domain.ExchangeRate, error) {
	var (
		r       domain.ExchangeRate
		rate    string
		inverse bool
	)
	err := tx.QueryRow(ctx, `
		SELECT base_currency, quote_currency, rate::text, provider, fetched_at, base_currency <> $1
		FROM exchange_rates
		WHERE (base_currency = $1 AND quote_currency = $2) OR (base_currency = $2 AND quote_currency = $1)
		ORDER BY base_currency <> $1 LIMIT 1`, base, quote).
		Scan(&r.Base, &r.Quote, &rate, &r.Provider, &r.FetchedAt, &inverse)
	if dbkit.IsNoRows(err) {
		return nil, nil //nolint:nilnil // no rate is not an error
	}
	if err != nil {
		return nil, fmt.Errorf("get rate: %w", err)
	}
	if r.Rate, err = money.ParseRate(rate); err != nil {
		return nil, fmt.Errorf("get rate: %w", err)
	}
	if inverse {
		if r.Rate, err = r.Rate.Inverse(); err != nil {
			return nil, fmt.Errorf("get rate: %w", err)
		}
		r.Base, r.Quote = base, quote
	}
	return &r, nil
}

// RecordFetch records a refresh attempt of base's rates; fetchErr is ""
// on success.
func (Pricing) RecordFetch(ctx context.Context, tx pgx.Tx, base, provider string, at time.Time, fetchErr string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO exchange_rate_fetches (base_currency, provider, last_success_at, last_attempt_at, last_error)
		VALUES ($1, $2, CASE WHEN $4 = '' THEN $3::timestamptz END, $3, $4)
		ON CONFLICT (base_currency) DO UPDATE SET provider = EXCLUDED.provider,
			last_success_at = COALESCE(EXCLUDED.last_success_at, exchange_rate_fetches.last_success_at),
			last_attempt_at = EXCLUDED.last_attempt_at, last_error = EXCLUDED.last_error,
			stale_reported_at = CASE WHEN $4 = '' THEN NULL ELSE exchange_rate_fetches.stale_reported_at END`,
		base, provider, at, fetchErr)
	if err != nil {
		return fmt.Errorf("record fetch: %w", err)
	}
	return nil
}

// FetchStatus returns base's refresh state locked for update, or nil.
func (Pricing) FetchStatus(ctx context.Context, tx pgx.Tx, base string) (*FetchStatus, error) {
	var s FetchStatus
	err := tx.QueryRow(ctx, `
		SELECT base_currency, provider, last_success_at, last_attempt_at, last_error, stale_reported_at
		FROM exchange_rate_fetches WHERE base_currency = $1 FOR UPDATE`, base).
		Scan(&s.Base, &s.Provider, &s.LastSuccessAt, &s.LastAttemptAt, &s.LastError, &s.StaleReportedAt)
	if dbkit.IsNoRows(err) {
		return nil, nil //nolint:nilnil // never fetched is not an error
	}
	if err != nil {
		return nil, fmt.Errorf("fetch status: %w", err)
	}
	return &s, nil
}

// MarkStaleReported records that staleness of base's rates was reported.
func (Pricing) MarkStaleReported(ctx context.Context, tx pgx.Tx, base string, at time.Time) error {
	if _, err := tx.Exec(ctx, `UPDATE exchange_rate_fetches SET stale_reported_at = $2 WHERE base_currency = $1`, base, at); err != nil {
		return fmt.Errorf("mark stale reported: %w", err)
	}
	return nil
}
