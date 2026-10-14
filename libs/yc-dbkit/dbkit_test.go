package dbkit

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPgx5URL(t *testing.T) {
	t.Parallel()
	got, err := pgx5URL("postgres://u:p@localhost:5432/db?sslmode=disable")
	require.NoError(t, err)
	assert.Equal(t, "pgx5://u:p@localhost:5432/db?sslmode=disable", got)

	_, err = pgx5URL("mysql://x")
	assert.Error(t, err)
}

func TestOperation(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "select", operation("SELECT 1"))
	assert.Equal(t, "insert", operation("insert into x values (1)"))
	assert.Equal(t, "query", operation("SET x = 1"))
	assert.Equal(t, "query", operation(""))
}

func TestWithTenantTxRejectsPlatformAndEmpty(t *testing.T) {
	t.Parallel()
	// Both checks run before the pool is touched.
	err := WithTenantTx(context.Background(), nil, PlatformTenantID, nil)
	require.ErrorIs(t, err, ErrPlatformTenant)

	err = WithTenantTx(context.Background(), nil, "", nil)
	require.Error(t, err)
}
