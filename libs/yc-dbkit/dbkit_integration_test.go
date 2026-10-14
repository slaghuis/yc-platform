//go:build integration

package dbkit_test

import (
	"context"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	testkit "github.com/slaghuis/yc-platform/libs/yc-testkit"
)

var migrations = fstest.MapFS{
	"001_notes.up.sql": {Data: []byte(`
CREATE TABLE notes (
    id        SERIAL PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    body      TEXT NOT NULL UNIQUE
);
ALTER TABLE notes ENABLE ROW LEVEL SECURITY;
ALTER TABLE notes FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON notes
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform')
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
`)},
	"001_notes.down.sql": {Data: []byte(`DROP TABLE notes;`)},
}

func countNotes(ctx context.Context, tx pgx.Tx) (int, error) {
	var n int
	err := tx.QueryRow(ctx, "SELECT count(*) FROM notes").Scan(&n)
	return n, err
}

func TestTenantIsolation(t *testing.T) {
	ctx := context.Background()
	url := testkit.Postgres(t)

	require.NoError(t, dbkit.Migrate(url, migrations))
	require.NoError(t, dbkit.Migrate(url, migrations), "second run is a no-op")

	cfg := dbkit.DefaultConfig()
	cfg.URL = url
	db, err := dbkit.Connect(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(db.Close)

	insert := func(tenant, body string) error {
		return dbkit.WithTenantTx(ctx, db, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "INSERT INTO notes (tenant_id, body) VALUES ($1, $2)", tenant, body)
			return err
		})
	}
	require.NoError(t, insert("shop-a", "a1"))
	require.NoError(t, insert("shop-a", "a2"))
	require.NoError(t, insert("shop-b", "b1"))

	t.Run("tenant sees only its rows", func(t *testing.T) {
		var n int
		require.NoError(t, dbkit.WithTenantTx(ctx, db, "shop-a", func(tx pgx.Tx) (err error) {
			n, err = countNotes(ctx, tx)
			return err
		}))
		assert.Equal(t, 2, n)
	})

	t.Run("cannot write another tenant's row", func(t *testing.T) {
		err := dbkit.WithTenantTx(ctx, db, "shop-a", func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "INSERT INTO notes (tenant_id, body) VALUES ('shop-b', 'sneaky')")
			return err
		})
		require.Error(t, err)
	})

	t.Run("platform sees all", func(t *testing.T) {
		var n int
		require.NoError(t, dbkit.WithPlatformTx(ctx, db, func(tx pgx.Tx) (err error) {
			n, err = countNotes(ctx, tx)
			return err
		}))
		assert.Equal(t, 3, n)
	})

	t.Run("no tenant sees nothing", func(t *testing.T) {
		var n int
		require.NoError(t, dbkit.InTx(ctx, db, func(tx pgx.Tx) (err error) {
			n, err = countNotes(ctx, tx)
			return err
		}))
		assert.Equal(t, 0, n)
	})

	t.Run("unique violation is classified", func(t *testing.T) {
		err := insert("shop-a", "a1")
		assert.True(t, dbkit.IsUniqueViolation(err))
	})
}
