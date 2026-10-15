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

// RefreshToken is a row of refresh_tokens.
type RefreshToken struct {
	ID         string
	TenantID   string
	UserID     string
	FamilyID   string
	ExpiresAt  time.Time
	RevokedAt  *time.Time
	ReplacedBy *string
}

// Sessions persists refresh tokens and logged-out access tokens.
type Sessions struct{}

// InsertRefresh stores a refresh token by its hash and returns its ID.
func (Sessions) InsertRefresh(ctx context.Context, tx pgx.Tx, t RefreshToken, hash []byte) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO refresh_tokens (tenant_id, user_id, family_id, token_hash, expires_at)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		t.TenantID, t.UserID, t.FamilyID, hash, t.ExpiresAt).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert refresh token: %w", err)
	}
	return id, nil
}

// GetRefreshForUpdate returns the refresh token with hash, locked, or a
// not_found error.
func (Sessions) GetRefreshForUpdate(ctx context.Context, tx pgx.Tx, hash []byte) (RefreshToken, error) {
	var t RefreshToken
	err := tx.QueryRow(ctx, `
		SELECT id, tenant_id, user_id, family_id, expires_at, revoked_at, replaced_by
		  FROM refresh_tokens WHERE token_hash = $1 FOR UPDATE`, hash).
		Scan(&t.ID, &t.TenantID, &t.UserID, &t.FamilyID, &t.ExpiresAt, &t.RevokedAt, &t.ReplacedBy)
	if dbkit.IsNoRows(err) {
		return RefreshToken{}, ycerrors.NotFound("RefreshToken", "")
	}
	if err != nil {
		return RefreshToken{}, fmt.Errorf("get refresh token: %w", err)
	}
	return t, nil
}

// MarkReplaced records that refresh token id was rotated into next.
func (Sessions) MarkReplaced(ctx context.Context, tx pgx.Tx, id, next string) error {
	_, err := tx.Exec(ctx, `UPDATE refresh_tokens SET replaced_by = $2 WHERE id = $1`, id, next)
	if err != nil {
		return fmt.Errorf("mark refresh token replaced: %w", err)
	}
	return nil
}

// RevokeFamily revokes every live token of a login.
func (Sessions) RevokeFamily(ctx context.Context, tx pgx.Tx, familyID string) error {
	_, err := tx.Exec(ctx,
		`UPDATE refresh_tokens SET revoked_at = NOW() WHERE family_id = $1 AND revoked_at IS NULL`, familyID)
	if err != nil {
		return fmt.Errorf("revoke refresh family: %w", err)
	}
	return nil
}

// RevokeUser revokes every live refresh token of a user.
func (Sessions) RevokeUser(ctx context.Context, tx pgx.Tx, userID string) error {
	_, err := tx.Exec(ctx,
		`UPDATE refresh_tokens SET revoked_at = NOW() WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	if err != nil {
		return fmt.Errorf("revoke user refresh tokens: %w", err)
	}
	return nil
}

// RevokeAccess records a logged-out access token until it expires.
func (Sessions) RevokeAccess(ctx context.Context, tx pgx.Tx, tenantID, jti string, expiresAt time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO revoked_access_tokens (jti, tenant_id, expires_at) VALUES ($1, $2, $3)
		ON CONFLICT (jti) DO NOTHING`, jti, tenantID, expiresAt)
	if err != nil {
		return fmt.Errorf("revoke access token: %w", err)
	}
	return nil
}

// AccessRevoked reports whether access token jti was logged out.
func (Sessions) AccessRevoked(ctx context.Context, tx pgx.Tx, jti string) (bool, error) {
	var revoked bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM revoked_access_tokens WHERE jti = $1)`, jti).Scan(&revoked)
	if err != nil {
		return false, fmt.Errorf("check revoked access token: %w", err)
	}
	return revoked, nil
}

// OAuthState is a pending authorisation.
type OAuthState struct {
	TenantID     string
	Provider     string
	CodeVerifier string
	RedirectURL  string
	VisitorID    string // "" when the flow did not start from a visitor
	ExpiresAt    time.Time
}

// OAuth persists provider links and pending authorisations.
type OAuth struct{}

// InsertState stores a pending authorisation by its state hash.
func (OAuth) InsertState(ctx context.Context, tx pgx.Tx, s OAuthState, hash []byte) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO oauth_states (state_hash, tenant_id, provider, code_verifier, redirect_url, visitor_id, expires_at)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, '')::uuid, $7)`,
		hash, s.TenantID, s.Provider, s.CodeVerifier, s.RedirectURL, s.VisitorID, s.ExpiresAt)
	if err != nil {
		return fmt.Errorf("insert oauth state: %w", err)
	}
	return nil
}

// TakeState deletes and returns the pending authorisation with hash, or a
// not_found error. Expired rows are returned too; the caller checks.
func (OAuth) TakeState(ctx context.Context, tx pgx.Tx, hash []byte) (OAuthState, error) {
	var s OAuthState
	err := tx.QueryRow(ctx, `
		DELETE FROM oauth_states WHERE state_hash = $1
		RETURNING tenant_id, provider, code_verifier, redirect_url, COALESCE(visitor_id::text, ''), expires_at`, hash).
		Scan(&s.TenantID, &s.Provider, &s.CodeVerifier, &s.RedirectURL, &s.VisitorID, &s.ExpiresAt)
	if dbkit.IsNoRows(err) {
		return OAuthState{}, ycerrors.NotFound("OAuthState", "")
	}
	if err != nil {
		return OAuthState{}, fmt.Errorf("take oauth state: %w", err)
	}
	return s, nil
}

// DeleteExpiredStates removes abandoned authorisations.
func (OAuth) DeleteExpiredStates(ctx context.Context, tx pgx.Tx, now time.Time) error {
	_, err := tx.Exec(ctx, `DELETE FROM oauth_states WHERE expires_at < $1`, now)
	if err != nil {
		return fmt.Errorf("delete expired oauth states: %w", err)
	}
	return nil
}

// LinkedUser returns the user linked to the provider account, or a
// not_found error.
func (OAuth) LinkedUser(ctx context.Context, tx pgx.Tx, tenantID, provider, subject string) (string, error) {
	var userID string
	err := tx.QueryRow(ctx, `
		SELECT user_id FROM oauth_identities WHERE tenant_id = $1 AND provider = $2 AND provider_sub = $3`,
		tenantID, provider, subject).Scan(&userID)
	if dbkit.IsNoRows(err) {
		return "", ycerrors.NotFound("OAuthIdentity", provider)
	}
	if err != nil {
		return "", fmt.Errorf("get oauth identity: %w", err)
	}
	return userID, nil
}

// Link links a provider account to user. A user holds at most one account
// per provider.
func (OAuth) Link(ctx context.Context, tx pgx.Tx, user domain.User, provider string, p domain.OAuthProfile) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO oauth_identities (tenant_id, user_id, provider, provider_sub, email_at_link)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''))`,
		user.TenantID, user.ID, provider, p.Subject, p.Email)
	if dbkit.IsUniqueViolation(err) {
		return ycerrors.Conflict("This account is already linked to another " + provider + " account")
	}
	if err != nil {
		return fmt.Errorf("link oauth identity: %w", err)
	}
	return nil
}

// UnlinkAll removes every provider account linked to user and returns how
// many there were.
func (OAuth) UnlinkAll(ctx context.Context, tx pgx.Tx, userID string) (int64, error) {
	tag, err := tx.Exec(ctx, `DELETE FROM oauth_identities WHERE user_id = $1`, userID)
	if err != nil {
		return 0, fmt.Errorf("unlink oauth identities: %w", err)
	}
	return tag.RowsAffected(), nil
}

// PasswordResets persists reset tokens.
type PasswordResets struct{}

// DeleteUser removes every reset token of user.
func (PasswordResets) DeleteUser(ctx context.Context, tx pgx.Tx, userID string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM password_reset_tokens WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("delete password reset tokens: %w", err)
	}
	return nil
}

// Insert stores a reset token by its hash.
func (PasswordResets) Insert(ctx context.Context, tx pgx.Tx, id, tenantID, userID string, hash []byte, expiresAt time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO password_reset_tokens (token_hash, id, tenant_id, user_id, expires_at)
		VALUES ($1, $2, $3, $4, $5)`, hash, id, tenantID, userID, expiresAt)
	if err != nil {
		return fmt.Errorf("insert password reset token: %w", err)
	}
	return nil
}

// Use marks tenantID's unused, unexpired reset token with hash used and
// returns its user, or a not_found error.
func (PasswordResets) Use(ctx context.Context, tx pgx.Tx, tenantID string, hash []byte, now time.Time) (string, error) {
	var userID string
	err := tx.QueryRow(ctx, `
		UPDATE password_reset_tokens SET used_at = $3
		 WHERE token_hash = $1 AND tenant_id = $2 AND used_at IS NULL AND expires_at > $3
		RETURNING user_id`, hash, tenantID, now).Scan(&userID)
	if dbkit.IsNoRows(err) {
		return "", ycerrors.NotFound("PasswordResetToken", "")
	}
	if err != nil {
		return "", fmt.Errorf("use password reset token: %w", err)
	}
	return userID, nil
}

// UseAll marks every outstanding reset token of user used.
func (PasswordResets) UseAll(ctx context.Context, tx pgx.Tx, userID string, now time.Time) error {
	_, err := tx.Exec(ctx,
		`UPDATE password_reset_tokens SET used_at = $2 WHERE user_id = $1 AND used_at IS NULL`, userID, now)
	if err != nil {
		return fmt.Errorf("use password reset tokens: %w", err)
	}
	return nil
}
