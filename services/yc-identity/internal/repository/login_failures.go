package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
)

// LoginFailures counts failed password sign-ins per shop and email, in
// fixed windows.
type LoginFailures struct{}

// Count returns the failures for emailHash in the window that began at or
// after since.
func (LoginFailures) Count(ctx context.Context, tx pgx.Tx, tenantID string, emailHash []byte, since time.Time) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `
		SELECT failures FROM login_failures
		WHERE tenant_id = $1 AND email_hash = $2 AND window_start >= $3`, tenantID, emailHash, since).Scan(&n)
	if dbkit.IsNoRows(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("count login failures: %w", err)
	}
	return n, nil
}

// Record adds a failure for emailHash, starting a new window at now when
// the current one began before since, and drops the shop's expired rows.
func (LoginFailures) Record(ctx context.Context, tx pgx.Tx, tenantID string, emailHash []byte, now, since time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO login_failures (tenant_id, email_hash, failures, window_start)
		VALUES ($1, $2, 1, $3)
		ON CONFLICT (tenant_id, email_hash) DO UPDATE SET
			failures = CASE WHEN login_failures.window_start < $4 THEN 1 ELSE login_failures.failures + 1 END,
			window_start = CASE WHEN login_failures.window_start < $4 THEN $3 ELSE login_failures.window_start END`,
		tenantID, emailHash, now, since)
	if err != nil {
		return fmt.Errorf("record login failure: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM login_failures WHERE tenant_id = $1 AND window_start < $2`, tenantID, since); err != nil {
		return fmt.Errorf("prune login failures: %w", err)
	}
	return nil
}

// Clear forgets emailHash's failures after a successful sign-in.
func (LoginFailures) Clear(ctx context.Context, tx pgx.Tx, tenantID string, emailHash []byte) error {
	if _, err := tx.Exec(ctx, `DELETE FROM login_failures WHERE tenant_id = $1 AND email_hash = $2`, tenantID, emailHash); err != nil {
		return fmt.Errorf("clear login failures: %w", err)
	}
	return nil
}
