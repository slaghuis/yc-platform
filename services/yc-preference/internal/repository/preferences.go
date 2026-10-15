// Package repository is yc-preference's Postgres access. Every method runs
// in the caller's transaction, so RLS (app.tenant_id) scopes the rows.
package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	"github.com/slaghuis/yc-platform/services/yc-preference/internal/domain"
)

// Preferences reads and writes the preference tables.
type Preferences struct{}

// Defaults returns tenantID's defaults, or false when it has none.
func (Preferences) Defaults(ctx context.Context, tx pgx.Tx, tenantID string) (domain.Defaults, bool, error) {
	var d domain.Defaults
	err := tx.QueryRow(ctx, `SELECT display_currency, locale, updated_at FROM preference_defaults
		WHERE tenant_id = $1`, tenantID).Scan(&d.DisplayCurrency, &d.Locale, &d.UpdatedAt)
	if dbkit.IsNoRows(err) {
		return domain.Defaults{}, false, nil
	}
	if err != nil {
		return domain.Defaults{}, false, fmt.Errorf("get defaults: %w", err)
	}
	return d, true, nil
}

// PutDefaults creates or replaces tenantID's defaults.
func (Preferences) PutDefaults(ctx context.Context, tx pgx.Tx, tenantID string, d domain.Defaults, now time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO preference_defaults (tenant_id, display_currency, locale, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $4)
		ON CONFLICT (tenant_id) DO UPDATE SET display_currency = EXCLUDED.display_currency,
			locale = EXCLUDED.locale, updated_at = EXCLUDED.updated_at`,
		tenantID, d.DisplayCurrency, d.Locale, now)
	if err != nil {
		return fmt.Errorf("put defaults: %w", err)
	}
	return nil
}

// InitDefaults creates tenantID's defaults unless they exist.
func (Preferences) InitDefaults(ctx context.Context, tx pgx.Tx, tenantID string, d domain.Defaults, now time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO preference_defaults (tenant_id, display_currency, locale, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $4) ON CONFLICT (tenant_id) DO NOTHING`,
		tenantID, d.DisplayCurrency, d.Locale, now)
	if err != nil {
		return fmt.Errorf("init defaults: %w", err)
	}
	return nil
}

// Get returns userID's stored preferences with their channels, locked when
// forUpdate, or false when there is no row.
func (Preferences) Get(ctx context.Context, tx pgx.Tx, tenantID, userID string, forUpdate bool) (domain.Preferences, bool, error) {
	q := `SELECT display_currency, locale, COALESCE(primary_channel, ''), updated_at FROM preferences
		WHERE tenant_id = $1 AND user_id = $2`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	p := domain.Preferences{UserID: userID, Stored: true}
	err := tx.QueryRow(ctx, q, tenantID, userID).Scan(&p.DisplayCurrency, &p.Locale, &p.PrimaryChannel, &p.UpdatedAt)
	if dbkit.IsNoRows(err) {
		return domain.Preferences{}, false, nil
	}
	if err != nil {
		return domain.Preferences{}, false, fmt.Errorf("get preferences: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT channel, address, verified, verified_at, created_at, updated_at
		FROM messaging_channels WHERE tenant_id = $1 AND user_id = $2 ORDER BY created_at, channel`, tenantID, userID)
	if err != nil {
		return domain.Preferences{}, false, fmt.Errorf("list channels: %w", err)
	}
	p.Channels, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.MessagingChannel, error) {
		var c domain.MessagingChannel
		err := r.Scan(&c.Channel, &c.Address, &c.Verified, &c.VerifiedAt, &c.CreatedAt, &c.UpdatedAt)
		return c, err
	})
	if err != nil {
		return domain.Preferences{}, false, fmt.Errorf("list channels: %w", err)
	}
	return p, true, nil
}

// Insert creates p's row unless one exists, reporting whether it did.
func (Preferences) Insert(ctx context.Context, tx pgx.Tx, tenantID string, p domain.Preferences, now time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO preferences (tenant_id, user_id, display_currency, locale, primary_channel, created_at, updated_at)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $6) ON CONFLICT (tenant_id, user_id) DO NOTHING`,
		tenantID, p.UserID, p.DisplayCurrency, p.Locale, string(p.PrimaryChannel), now)
	if err != nil {
		return false, fmt.Errorf("insert preferences: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Update writes p's settings.
func (Preferences) Update(ctx context.Context, tx pgx.Tx, tenantID string, p domain.Preferences, now time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE preferences SET display_currency = $3, locale = $4,
		primary_channel = NULLIF($5, ''), updated_at = $6 WHERE tenant_id = $1 AND user_id = $2`,
		tenantID, p.UserID, p.DisplayCurrency, p.Locale, string(p.PrimaryChannel), now)
	if err != nil {
		return fmt.Errorf("update preferences: %w", err)
	}
	return nil
}

// PutChannel adds a channel or replaces its address, which leaves it
// unverified.
func (Preferences) PutChannel(ctx context.Context, tx pgx.Tx, tenantID, userID string, c domain.Channel, address string, now time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO messaging_channels (tenant_id, user_id, channel, address, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $5)
		ON CONFLICT (tenant_id, user_id, channel) DO UPDATE SET address = EXCLUDED.address,
			verified = FALSE, verified_at = NULL, updated_at = EXCLUDED.updated_at`,
		tenantID, userID, string(c), address, now)
	if err != nil {
		return fmt.Errorf("put channel: %w", err)
	}
	return nil
}

// SetVerified marks a channel verified at verifiedAt, or unverified.
func (Preferences) SetVerified(ctx context.Context, tx pgx.Tx, tenantID, userID string, c domain.Channel, verified bool, verifiedAt *time.Time, now time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE messaging_channels SET verified = $4, verified_at = $5, updated_at = $6
		WHERE tenant_id = $1 AND user_id = $2 AND channel = $3`,
		tenantID, userID, string(c), verified, verifiedAt, now)
	if err != nil {
		return fmt.Errorf("set verified: %w", err)
	}
	return nil
}

// DeleteChannel removes a channel.
func (Preferences) DeleteChannel(ctx context.Context, tx pgx.Tx, tenantID, userID string, c domain.Channel) error {
	_, err := tx.Exec(ctx, `DELETE FROM messaging_channels WHERE tenant_id = $1 AND user_id = $2 AND channel = $3`,
		tenantID, userID, string(c))
	if err != nil {
		return fmt.Errorf("delete channel: %w", err)
	}
	return nil
}
