package testkit

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AssertTenantIsolation checks a migrated service database against
// ADR-017 rule 4: the service's role cannot bypass row-level security, and
// every table with a tenant_id column, other than those named in exempt,
// has forced RLS and only policies keyed on app.tenant_id. Every service
// exempts its outbox, which the relay drains across tenants (ADR-006).
func AssertTenantIsolation(tb testing.TB, db *pgxpool.Pool, exempt ...string) {
	tb.Helper()
	ctx := context.Background()

	var bypass bool
	if err := db.QueryRow(ctx,
		`SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&bypass); err != nil {
		tb.Fatalf("testkit: read role: %v", err)
	}
	if bypass {
		tb.Errorf("testkit: the service role bypasses row-level security")
	}

	rows, err := db.Query(ctx, `
		SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity,
		       COUNT(p.oid),
		       COUNT(p.oid) FILTER (WHERE pg_get_expr(p.polqual, p.polrelid) NOT LIKE '%app.tenant_id%'
		                            OR pg_get_expr(p.polwithcheck, p.polrelid) NOT LIKE '%app.tenant_id%')
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_policy p ON p.polrelid = c.oid
		WHERE n.nspname = current_schema() AND c.relkind IN ('r', 'p')
		  AND EXISTS (SELECT 1 FROM pg_attribute a
		              WHERE a.attrelid = c.oid AND a.attname = 'tenant_id' AND NOT a.attisdropped)
		GROUP BY c.relname, c.relrowsecurity, c.relforcerowsecurity
		ORDER BY c.relname`)
	if err != nil {
		tb.Fatalf("testkit: read tables: %v", err)
	}
	defer rows.Close()
	checked := 0
	for rows.Next() {
		var (
			table           string
			enabled, forced bool
			policies, loose int
		)
		if err := rows.Scan(&table, &enabled, &forced, &policies, &loose); err != nil {
			tb.Fatalf("testkit: scan table: %v", err)
		}
		if slices.Contains(exempt, table) {
			continue
		}
		checked++
		switch {
		case !enabled || !forced:
			tb.Errorf("testkit: %s has a tenant_id but row-level security is not enabled and forced", table)
		case policies == 0:
			tb.Errorf("testkit: %s has row-level security but no policy, so the service sees no rows", table)
		case loose > 0:
			tb.Errorf("testkit: %s has %d policies not keyed on app.tenant_id", table, loose)
		}
	}
	if err := rows.Err(); err != nil {
		tb.Fatalf("testkit: read tables: %v", err)
	}
	if checked == 0 {
		tb.Errorf("testkit: no tenant tables found; call AssertTenantIsolation after migrations")
	}
}
