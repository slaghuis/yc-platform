package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PutShopAdmin records a shop admin.
func (Messages) PutShopAdmin(ctx context.Context, tx pgx.Tx, tenantID, userID string, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO shop_admins (tenant_id, user_id, promoted_at) VALUES ($1, $2, $3)
		ON CONFLICT (tenant_id, user_id) DO UPDATE SET promoted_at = EXCLUDED.promoted_at`, tenantID, userID, at)
	if err != nil {
		return fmt.Errorf("put shop admin: %w", err)
	}
	return nil
}

// DeleteShopAdmin forgets a shop admin.
func (Messages) DeleteShopAdmin(ctx context.Context, tx pgx.Tx, tenantID, userID string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM shop_admins WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID); err != nil {
		return fmt.Errorf("delete shop admin: %w", err)
	}
	return nil
}

// ShopAdmins returns a shop's admins' user IDs.
func (Messages) ShopAdmins(ctx context.Context, tx pgx.Tx, tenantID string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT user_id FROM shop_admins WHERE tenant_id = $1 ORDER BY promoted_at, user_id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list shop admins: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list shop admins: %w", err)
	}
	return ids, nil
}

// PutSystemAdmin records an invited system admin (ADR-030).
func (Messages) PutSystemAdmin(ctx context.Context, tx pgx.Tx, userID, email string, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO system_admins (user_id, email, accepted_at, suspended_at) VALUES ($1, $2, $3, NULL)
		ON CONFLICT (user_id) DO UPDATE SET email = EXCLUDED.email, accepted_at = EXCLUDED.accepted_at, suspended_at = NULL`,
		userID, email, at)
	if err != nil {
		return fmt.Errorf("put system admin: %w", err)
	}
	return nil
}

// SuspendSystemAdmin removes an invited system admin from the alert
// audience while keeping the email for reactivation.
func (Messages) SuspendSystemAdmin(ctx context.Context, tx pgx.Tx, userID string, at time.Time) error {
	if _, err := tx.Exec(ctx, `UPDATE system_admins SET suspended_at = $2 WHERE user_id = $1`, userID, at); err != nil {
		return fmt.Errorf("suspend system admin: %w", err)
	}
	return nil
}

// ReactivateSystemAdmin restores a soft-suspended system admin to the
// alert audience.
func (Messages) ReactivateSystemAdmin(ctx context.Context, tx pgx.Tx, userID string) error {
	if _, err := tx.Exec(ctx, `UPDATE system_admins SET suspended_at = NULL WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("reactivate system admin: %w", err)
	}
	return nil
}

// DeleteSystemAdmin forgets an invited system admin.
func (Messages) DeleteSystemAdmin(ctx context.Context, tx pgx.Tx, userID string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM system_admins WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("delete system admin: %w", err)
	}
	return nil
}

// SystemAdminEmails returns active invited system admins' emails for
// alert delivery.
func (Messages) SystemAdminEmails(ctx context.Context, tx pgx.Tx) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT email FROM system_admins WHERE suspended_at IS NULL ORDER BY accepted_at, user_id`)
	if err != nil {
		return nil, fmt.Errorf("list system admins: %w", err)
	}
	emails, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list system admins: %w", err)
	}
	return emails, nil
}

// ForgetRecipient pseudonymises a deleted user's messages (ADR-005): the
// user becomes the pseudonym and the address, subject, body and context
// are removed. Queued messages are cancelled as failed. Their verification
// codes and shop admin entry are deleted. It returns how many messages and
// codes changed.
func (Messages) ForgetRecipient(ctx context.Context, tx pgx.Tx, tenantID, userID, pseudonym, reason string, now time.Time) (int64, error) {
	msgs, err := tx.Exec(ctx, `UPDATE messages SET recipient_user_id = $3, recipient_address = '', subject = '',
			body_rendered = '', context = '{}', updated_at = $5,
			last_error = CASE WHEN status = 'queued' THEN $4 ELSE last_error END,
			status = CASE WHEN status = 'queued' THEN 'failed' ELSE status END,
			next_attempt_at = NULL
		WHERE tenant_id = $1 AND recipient_user_id = $2`, tenantID, userID, pseudonym, reason, now)
	if err != nil {
		return 0, fmt.Errorf("pseudonymise messages: %w", err)
	}
	codes, err := tx.Exec(ctx, `DELETE FROM verifications WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID)
	if err != nil {
		return 0, fmt.Errorf("delete verifications: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM shop_admins WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID); err != nil {
		return 0, fmt.Errorf("delete shop admin: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM system_admins WHERE user_id = $1`, userID); err != nil {
		return 0, fmt.Errorf("delete system admin: %w", err)
	}
	return msgs.RowsAffected() + codes.RowsAffected(), nil
}
