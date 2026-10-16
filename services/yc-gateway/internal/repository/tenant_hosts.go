// Package repository persists yc-gateway's tables.
package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is satisfied by *pgxpool.Pool and pgx.Tx.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
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
// the tenant the hostname belongs to afterwards. Claiming clears a prior
// deleted_hosts row so the hostname can serve the new shop (ADR-029).
func (TenantHosts) Claim(ctx context.Context, q Querier, hostname, tenantID string) (string, error) {
	if _, err := q.Exec(ctx, `DELETE FROM deleted_hosts WHERE hostname = $1`, hostname); err != nil {
		return "", fmt.Errorf("clear deleted host %s: %w", hostname, err)
	}
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

// HostsOf returns every hostname currently mapped to tenantID.
func (TenantHosts) HostsOf(ctx context.Context, q Querier, tenantID string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT hostname FROM tenant_hosts WHERE tenant_id = $1`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list hosts of %s: %w", tenantID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var host string
		if err := rows.Scan(&host); err != nil {
			return nil, fmt.Errorf("scan host of %s: %w", tenantID, err)
		}
		out = append(out, host)
	}
	return out, rows.Err()
}

// DropTenant removes every hostname mapping for tenantID.
func (TenantHosts) DropTenant(ctx context.Context, q Querier, tenantID string) error {
	_, err := q.Exec(ctx, `DELETE FROM tenant_hosts WHERE tenant_id = $1`, tenantID)
	if err != nil {
		return fmt.Errorf("drop hosts of %s: %w", tenantID, err)
	}
	return nil
}

// MarkDeleted records hostnames that should answer 410 until reclaimed.
func (TenantHosts) MarkDeleted(ctx context.Context, q Querier, tenantID string, hostnames []string) error {
	for _, host := range hostnames {
		_, err := q.Exec(ctx, `
			INSERT INTO deleted_hosts (hostname, tenant_id) VALUES ($1, $2)
			ON CONFLICT (hostname) DO UPDATE SET tenant_id = EXCLUDED.tenant_id`, host, tenantID)
		if err != nil {
			return fmt.Errorf("mark deleted host %s: %w", host, err)
		}
	}
	return nil
}

// AllDeleted returns hostname → former tenant for 410 answers.
func (TenantHosts) AllDeleted(ctx context.Context, q Querier) (map[string]string, error) {
	rows, err := q.Query(ctx, `SELECT hostname, tenant_id FROM deleted_hosts`)
	if err != nil {
		return nil, fmt.Errorf("list deleted hosts: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var host, tenant string
		if err := rows.Scan(&host, &tenant); err != nil {
			return nil, fmt.Errorf("scan deleted host: %w", err)
		}
		out[host] = tenant
	}
	return out, rows.Err()
}

// SuspendedTenants is the durable suspended set (ADR-029 rule 5).
type SuspendedTenants struct{}

// All returns every suspended tenant ID.
func (SuspendedTenants) All(ctx context.Context, q Querier) (map[string]string, error) {
	rows, err := q.Query(ctx, `SELECT tenant_id, reason FROM suspended_tenants`)
	if err != nil {
		return nil, fmt.Errorf("list suspended tenants: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, reason string
		if err := rows.Scan(&id, &reason); err != nil {
			return nil, fmt.Errorf("scan suspended tenant: %w", err)
		}
		out[id] = reason
	}
	return out, rows.Err()
}

// Put records a suspension.
func (SuspendedTenants) Put(ctx context.Context, q Querier, tenantID, reason string) error {
	_, err := q.Exec(ctx, `
		INSERT INTO suspended_tenants (tenant_id, reason) VALUES ($1, $2)
		ON CONFLICT (tenant_id) DO UPDATE SET reason = EXCLUDED.reason`, tenantID, reason)
	if err != nil {
		return fmt.Errorf("suspend tenant %s: %w", tenantID, err)
	}
	return nil
}

// Delete clears a suspension.
func (SuspendedTenants) Delete(ctx context.Context, q Querier, tenantID string) error {
	_, err := q.Exec(ctx, `DELETE FROM suspended_tenants WHERE tenant_id = $1`, tenantID)
	if err != nil {
		return fmt.Errorf("reinstate tenant %s: %w", tenantID, err)
	}
	return nil
}
