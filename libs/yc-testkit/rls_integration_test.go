//go:build integration

package testkit_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	testkit "github.com/slaghuis/yc-platform/libs/yc-testkit"
)

// recorder collects the errors AssertTenantIsolation reports.
type recorder struct {
	testing.TB
	errs []string
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

func TestAssertTenantIsolation(t *testing.T) {
	ctx := context.Background()
	db, err := pgxpool.New(ctx, testkit.Postgres(t))
	require.NoError(t, err)
	t.Cleanup(db.Close)

	const policy = `CREATE POLICY tenant_isolation ON %s
		USING (tenant_id = current_setting('app.tenant_id', true))
		WITH CHECK (tenant_id = current_setting('app.tenant_id', true))`
	for _, stmt := range []string{
		`CREATE TABLE good (tenant_id TEXT NOT NULL)`,
		`ALTER TABLE good ENABLE ROW LEVEL SECURITY`,
		`ALTER TABLE good FORCE ROW LEVEL SECURITY`,
		fmt.Sprintf(policy, "good"),
		`CREATE TABLE outbox (tenant_id TEXT NOT NULL)`,
		`CREATE TABLE platform_only (id INT)`,
	} {
		_, err := db.Exec(ctx, stmt)
		require.NoError(t, err, stmt)
	}

	check := func() []string {
		r := &recorder{TB: t}
		testkit.AssertTenantIsolation(r, db, "outbox")
		return r.errs
	}
	assert.Empty(t, check(), "forced RLS with a tenant policy, an exempt outbox and a table without tenant_id")

	for _, tc := range []struct {
		name  string
		setup []string
		want  string
	}{
		{"no RLS", nil, "bad has a tenant_id but row-level security is not enabled and forced"},
		{"RLS not forced", []string{
			`ALTER TABLE bad ENABLE ROW LEVEL SECURITY`,
			fmt.Sprintf(policy, "bad"),
		}, "bad has a tenant_id but row-level security is not enabled and forced"},
		{"no policy", []string{
			`ALTER TABLE bad ENABLE ROW LEVEL SECURITY`,
			`ALTER TABLE bad FORCE ROW LEVEL SECURITY`,
		}, "bad has row-level security but no policy, so the service sees no rows"},
		{"policy not keyed on the tenant", []string{
			`ALTER TABLE bad ENABLE ROW LEVEL SECURITY`,
			`ALTER TABLE bad FORCE ROW LEVEL SECURITY`,
			`CREATE POLICY everyone ON bad USING (true)`,
		}, "bad has 1 policies not keyed on app.tenant_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, stmt := range append([]string{`CREATE TABLE bad (tenant_id TEXT)`}, tc.setup...) {
				_, err := db.Exec(ctx, stmt)
				require.NoError(t, err, stmt)
			}
			t.Cleanup(func() { _, _ = db.Exec(ctx, `DROP TABLE bad`) })

			assert.Equal(t, []string{"testkit: " + tc.want}, check())
		})
	}
}
