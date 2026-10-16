package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ShopHosts projects each shop's primary hostname from tenant lifecycle
// events so invite links can be built without calling yc-tenant.
type ShopHosts struct{}

// Put upserts a shop's primary hostname.
func (ShopHosts) Put(ctx context.Context, tx pgx.Tx, tenantID, hostname string, at time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO shop_hosts (tenant_id, primary_hostname, updated_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (tenant_id) DO UPDATE SET
			primary_hostname = EXCLUDED.primary_hostname,
			updated_at = EXCLUDED.updated_at`,
		tenantID, hostname, at)
	if err != nil {
		return fmt.Errorf("put shop host: %w", err)
	}
	return nil
}

// Primary returns a shop's primary hostname, if known.
func (ShopHosts) Primary(ctx context.Context, tx pgx.Tx, tenantID string) (string, bool, error) {
	var host string
	err := tx.QueryRow(ctx, `SELECT primary_hostname FROM shop_hosts WHERE tenant_id = $1`, tenantID).Scan(&host)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("shop primary host: %w", err)
	}
	return host, true, nil
}

// Delete drops a shop's projection when the shop is deleted.
func (ShopHosts) Delete(ctx context.Context, tx pgx.Tx, tenantID string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM shop_hosts WHERE tenant_id = $1`, tenantID); err != nil {
		return fmt.Errorf("delete shop host: %w", err)
	}
	return nil
}
