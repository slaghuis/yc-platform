package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/domain"
)

// Invitations persists admin invitations (ADR-030).
type Invitations struct{}

const invitationColumns = `id, tenant_id, email, role, status, COALESCE(display_name, ''),
	expires_at, invited_by::text, COALESCE(accepted_user_id::text, ''), created_at, updated_at, revoked_at`

// ErrPendingInvite is returned when the email already has a pending invite.
var ErrPendingInvite = ycerrors.Conflict("An invitation is already pending for this email")

// Insert stores a new pending invitation.
func (Invitations) Insert(ctx context.Context, tx pgx.Tx, inv domain.Invitation, tokenHash []byte) (domain.Invitation, error) {
	row := tx.QueryRow(ctx, `
		INSERT INTO invitations (id, tenant_id, email, role, status, display_name, token_hash, expires_at, invited_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7, $8, $9::uuid, $10, $10)
		RETURNING `+invitationColumns,
		inv.ID, inv.TenantID, inv.Email, inv.Role, domain.InvitationPending,
		inv.DisplayName, tokenHash, inv.ExpiresAt, inv.InvitedBy, inv.CreatedAt)
	out, err := scanInvitation(row)
	if dbkit.IsUniqueViolation(err) {
		return domain.Invitation{}, ErrPendingInvite
	}
	if err != nil {
		return domain.Invitation{}, fmt.Errorf("insert invitation: %w", err)
	}
	return out, nil
}

// Get returns invitation id, or not_found.
func (Invitations) Get(ctx context.Context, tx pgx.Tx, id string) (domain.Invitation, error) {
	inv, err := scanInvitation(tx.QueryRow(ctx,
		`SELECT `+invitationColumns+` FROM invitations WHERE id = $1`, id))
	if dbkit.IsNoRows(err) {
		return domain.Invitation{}, ycerrors.NotFound("Invitation", id)
	}
	if err != nil {
		return domain.Invitation{}, fmt.Errorf("get invitation: %w", err)
	}
	return inv, nil
}

// GetByTokenHash returns the pending, unexpired invitation with hash in
// tenantID, or not_found.
func (Invitations) GetByTokenHash(ctx context.Context, tx pgx.Tx, tenantID string, hash []byte, now time.Time) (domain.Invitation, error) {
	inv, err := scanInvitation(tx.QueryRow(ctx, `
		SELECT `+invitationColumns+` FROM invitations
		 WHERE token_hash = $1 AND tenant_id = $2 AND status = $3 AND expires_at > $4
		 FOR UPDATE`, hash, tenantID, domain.InvitationPending, now))
	if dbkit.IsNoRows(err) {
		return domain.Invitation{}, ycerrors.NotFound("Invitation", "")
	}
	if err != nil {
		return domain.Invitation{}, fmt.Errorf("get invitation by token: %w", err)
	}
	return inv, nil
}

// Resend replaces the token and extends expiry of a pending invitation.
func (Invitations) Resend(ctx context.Context, tx pgx.Tx, id string, tokenHash []byte, expiresAt, now time.Time) (domain.Invitation, error) {
	inv, err := scanInvitation(tx.QueryRow(ctx, `
		UPDATE invitations
		   SET token_hash = $2, expires_at = $3, updated_at = $4
		 WHERE id = $1 AND status = $5
		RETURNING `+invitationColumns,
		id, tokenHash, expiresAt, now, domain.InvitationPending))
	if dbkit.IsNoRows(err) {
		return domain.Invitation{}, ycerrors.NotFound("Invitation", id)
	}
	if err != nil {
		return domain.Invitation{}, fmt.Errorf("resend invitation: %w", err)
	}
	return inv, nil
}

// Accept marks a pending invitation accepted, records the user, and clears
// its token.
func (Invitations) Accept(ctx context.Context, tx pgx.Tx, id, userID string, now time.Time) error {
	tag, err := tx.Exec(ctx, `
		UPDATE invitations
		   SET status = $2, token_hash = NULL, accepted_user_id = $3::uuid, updated_at = $4
		 WHERE id = $1 AND status = $5`,
		id, domain.InvitationAccepted, userID, now, domain.InvitationPending)
	if err != nil {
		return fmt.Errorf("accept invitation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ycerrors.NotFound("Invitation", id)
	}
	return nil
}

// Revoke marks a pending invitation revoked and clears its token.
func (Invitations) Revoke(ctx context.Context, tx pgx.Tx, id string, now time.Time) (domain.Invitation, error) {
	inv, err := scanInvitation(tx.QueryRow(ctx, `
		UPDATE invitations
		   SET status = $2, token_hash = NULL, revoked_at = $3, updated_at = $3
		 WHERE id = $1 AND status = $4
		RETURNING `+invitationColumns,
		id, domain.InvitationRevoked, now, domain.InvitationPending))
	if dbkit.IsNoRows(err) {
		return domain.Invitation{}, ycerrors.NotFound("Invitation", id)
	}
	if err != nil {
		return domain.Invitation{}, fmt.Errorf("revoke invitation: %w", err)
	}
	return inv, nil
}

// InvitationFilter narrows List. Empty fields match everything.
type InvitationFilter struct {
	TenantID string
	Status   domain.InvitationStatus
}

// InvitationCursor is the keyset position after the last invitation of a page.
type InvitationCursor struct {
	CreatedAt time.Time `json:"c"`
	ID        string    `json:"i"`
}

// List returns up to limit invitations matching f, newest first.
func (Invitations) List(ctx context.Context, tx pgx.Tx, f InvitationFilter, after *InvitationCursor, limit int, now time.Time) ([]domain.Invitation, error) {
	q := `SELECT ` + invitationColumns + ` FROM invitations WHERE ($1 = '' OR tenant_id = $1)`
	args := []any{f.TenantID}
	switch f.Status {
	case domain.InvitationPending:
		q += ` AND status = $2 AND expires_at > $3`
		args = append(args, domain.InvitationPending, now)
	case domain.InvitationExpired:
		q += ` AND ((status = $2) OR (status = $3 AND expires_at <= $4))`
		args = append(args, domain.InvitationExpired, domain.InvitationPending, now)
	case domain.InvitationAccepted, domain.InvitationRevoked:
		q += fmt.Sprintf(` AND status = $%d`, len(args)+1)
		args = append(args, f.Status)
	case "":
	default:
		q += fmt.Sprintf(` AND status = $%d`, len(args)+1)
		args = append(args, f.Status)
	}
	if after != nil {
		q += fmt.Sprintf(` AND (created_at, id) < ($%d, $%d)`, len(args)+1, len(args)+2)
		args = append(args, after.CreatedAt, after.ID)
	}
	q += fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT %d`, limit)

	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list invitations: %w", err)
	}
	invs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Invitation, error) {
		return scanInvitation(r)
	})
	if err != nil {
		return nil, fmt.Errorf("list invitations: %w", err)
	}
	for i := range invs {
		invs[i].Status = invs[i].EffectiveStatus(now)
	}
	return invs, nil
}

func scanInvitation(row pgx.Row) (domain.Invitation, error) {
	var inv domain.Invitation
	err := row.Scan(
		&inv.ID, &inv.TenantID, &inv.Email, &inv.Role, &inv.Status, &inv.DisplayName,
		&inv.ExpiresAt, &inv.InvitedBy, &inv.AcceptedUserID, &inv.CreatedAt, &inv.UpdatedAt, &inv.RevokedAt,
	)
	return inv, err
}
