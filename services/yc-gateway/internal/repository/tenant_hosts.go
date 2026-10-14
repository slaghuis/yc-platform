// Package repository persists yc-gateway's tables.
package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Querier is satisfied by *pgxpool.Pool and pgx.Tx.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// TenantHosts reads and writes tenant_hosts. The table is platform-wide
// and has no RLS (ADR-020).
type TenantHosts struct{}

// All returns every hostname → tenant mapping.
func (TenantHosts) All(ctx context.Context, q Querier) (map[string]string, error) {
	rows, err := q.Query(ctx, `SELECT hostname, tenant_id FROM tenant_hosts`)
	if err != nil {
		return nil, fmt.Errorf("list tenant hosts: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var host, tenant string
		if err := rows.Scan(&host, &tenant); err != nil {
			return nil, fmt.Errorf("scan tenant host: %w", err)
		}
		out[host] = tenant
	}
	return out, rows.Err()
}

// Claim maps hostname to tenantID unless it is already mapped, and returns
// the tenant the hostname belongs to afterwards.
func (TenantHosts) Claim(ctx context.Context, q Querier, hostname, tenantID string) (string, error) {
	var owner string
	err := q.QueryRow(ctx, `
		INSERT INTO tenant_hosts (hostname, tenant_id) VALUES ($1, $2)
		ON CONFLICT (hostname) DO UPDATE SET hostname = EXCLUDED.hostname
		RETURNING tenant_id
	`, hostname, tenantID).Scan(&owner)
	if err != nil {
		return "", fmt.Errorf("claim tenant host %s: %w", hostname, err)
	}
	return owner, nil
}
