package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PutShop upserts a shop's name and primary hostname for message links.
func (Messages) PutShop(ctx context.Context, tx pgx.Tx, tenantID, name, primaryHostname string, at time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO shop_tenants (tenant_id, name, primary_hostname, updated_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id) DO UPDATE SET
			name = EXCLUDED.name,
			primary_hostname = EXCLUDED.primary_hostname,
			updated_at = EXCLUDED.updated_at`,
		tenantID, name, primaryHostname, at)
	if err != nil {
		return fmt.Errorf("put shop: %w", err)
	}
	return nil
}

// ShopPrimaryHost returns a shop's primary hostname, if known.
func (Messages) ShopPrimaryHost(ctx context.Context, tx pgx.Tx, tenantID string) (string, bool, error) {
	var host string
	err := tx.QueryRow(ctx, `SELECT primary_hostname FROM shop_tenants WHERE tenant_id = $1`, tenantID).Scan(&host)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("shop primary host: %w", err)
	}
	return host, true, nil
}

// DeleteShop drops a shop's projection when the shop is deleted.
func (Messages) DeleteShop(ctx context.Context, tx pgx.Tx, tenantID string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM shop_tenants WHERE tenant_id = $1`, tenantID); err != nil {
		return fmt.Errorf("delete shop: %w", err)
	}
	return nil
}
