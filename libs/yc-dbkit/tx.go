package dbkit

import (
	"context"
	stderrors "errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PlatformTenantID is the tenant whose RLS context sees every tenant's rows
// (ADR-008). Only WithPlatformTx sets it.
const PlatformTenantID = "_platform"

// ErrPlatformTenant is returned by WithTenantTx for the platform tenant:
// cross-tenant access must be explicit.
var ErrPlatformTenant = stderrors.New("dbkit: use WithPlatformTx for the _platform tenant")

// TxFunc is the body of a transaction. Returning an error rolls back.
type TxFunc func(tx pgx.Tx) error

// WithTenantTx runs fn in a transaction whose RLS context is tenantID
// (`app.tenant_id`, transaction-local), so tenant-scoped tables only show
// that tenant's rows (ADR-017).
//
// It refuses the platform tenant. Policies let `_platform` see every
// tenant, so a handler that passes an event's tenant straight through would
// otherwise read across tenants for any platform event.
func WithTenantTx(ctx context.Context, db *pgxpool.Pool, tenantID string, fn TxFunc) error {
	if tenantID == "" {
		return stderrors.New("dbkit: tenant id is required")
	}
	if tenantID == PlatformTenantID {
		return ErrPlatformTenant
	}
	return withRLSTx(ctx, db, tenantID, fn)
}

// WithPlatformTx runs fn with the platform RLS context, which sees every
// tenant's rows. Use it only for system-admin reads and platform jobs.
func WithPlatformTx(ctx context.Context, db *pgxpool.Pool, fn TxFunc) error {
	return withRLSTx(ctx, db, PlatformTenantID, fn)
}

// InTx runs fn in a transaction with no tenant context. RLS-protected
// tables return no rows; use it for platform-wide tables only.
func InTx(ctx context.Context, db *pgxpool.Pool, fn TxFunc) error {
	return pgx.BeginFunc(ctx, db, fn)
}

func withRLSTx(ctx context.Context, db *pgxpool.Pool, tenantID string, fn TxFunc) error {
	return pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
			return fmt.Errorf("dbkit: set tenant: %w", err)
		}
		return fn(tx)
	})
}

// Postgres error codes the repository layer translates.
const (
	codeUniqueViolation     = "23505"
	codeForeignKeyViolation = "23503"
)

// IsUniqueViolation reports a unique constraint violation (translate to
// conflict).
func IsUniqueViolation(err error) bool { return pgCode(err) == codeUniqueViolation }

// IsForeignKeyViolation reports a foreign key violation (usually a bug).
func IsForeignKeyViolation(err error) bool { return pgCode(err) == codeForeignKeyViolation }

// IsNoRows reports pgx.ErrNoRows.
func IsNoRows(err error) bool { return stderrors.Is(err, pgx.ErrNoRows) }

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if stderrors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}
