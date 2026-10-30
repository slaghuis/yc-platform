// Package repository persists yc-identity's aggregates. Methods take the
// caller's transaction, whose RLS tenant context scopes every query.
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

// Users reads and writes the users table.
type Users struct{}

const userColumns = "id, tenant_id, COALESCE(email, ''), COALESCE(display_name, ''), COALESCE(phone_e164, ''), COALESCE(profile_image_url, ''), COALESCE(password_hash, ''), role, status, created_at"

// ErrEmailTaken is returned when (tenant_id, email) already exists.
var ErrEmailTaken = ycerrors.Conflict("Email is already registered")

// CreateVisitor inserts a visitor for tenantID.
func (Users) CreateVisitor(ctx context.Context, tx pgx.Tx, tenantID string) (domain.User, error) {
	row := tx.QueryRow(ctx,
		`INSERT INTO users (tenant_id, role) VALUES ($1, $2) RETURNING `+userColumns,
		tenantID, domain.RoleVisitor)
	u, err := scanUser(row)
	if err != nil {
		return domain.User{}, fmt.Errorf("insert visitor: %w", err)
	}
	return u, nil
}

// Get returns the user with id, or a not_found error when it does not exist
// or belongs to another tenant.
func (Users) Get(ctx context.Context, tx pgx.Tx, id string) (domain.User, error) {
	u, err := scanUser(tx.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id))
	if dbkit.IsNoRows(err) {
		return domain.User{}, ycerrors.NotFound("User", id)
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("get user: %w", err)
	}
	return u, nil
}

// GetForUpdate is Get with the row locked until the transaction ends.
func (Users) GetForUpdate(ctx context.Context, tx pgx.Tx, id string) (domain.User, error) {
	u, err := scanUser(tx.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1 FOR UPDATE`, id))
	if dbkit.IsNoRows(err) {
		return domain.User{}, ycerrors.NotFound("User", id)
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("get user for update: %w", err)
	}
	return u, nil
}

// GetByEmail returns the user with email in tenantID. The tenant is
// filtered explicitly because a _platform transaction sees every tenant.
func (Users) GetByEmail(ctx context.Context, tx pgx.Tx, tenantID, email string) (domain.User, error) {
	u, err := scanUser(tx.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users WHERE tenant_id = $1 AND email = $2`, tenantID, email))
	if dbkit.IsNoRows(err) {
		return domain.User{}, ycerrors.NotFound("User", email)
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("get user by email: %w", err)
	}
	return u, nil
}

// Register turns the active visitor id into a customer with credentials.
// It returns a not_found error when id is not an active visitor.
func (Users) Register(ctx context.Context, tx pgx.Tx, id string, reg domain.Registration, hash string) (domain.User, error) {
	u, err := scanUser(tx.QueryRow(ctx, `
		UPDATE users
		   SET email = $2, password_hash = $3, display_name = NULLIF($4, ''),
		       role = $5, updated_at = NOW()
		 WHERE id = $1 AND role = $6 AND status = 'active'
		RETURNING `+userColumns,
		id, reg.Email, hash, reg.DisplayName, domain.RoleCustomer, domain.RoleVisitor))
	switch {
	case dbkit.IsNoRows(err):
		return domain.User{}, ycerrors.NotFound("Visitor", id)
	case dbkit.IsUniqueViolation(err):
		return domain.User{}, ErrEmailTaken
	case err != nil:
		return domain.User{}, fmt.Errorf("register visitor: %w", err)
	}
	return u, nil
}

// Create inserts a user with credentials: a customer registering without a
// visitor, or the bootstrap system admin.
func (Users) Create(ctx context.Context, tx pgx.Tx, tenantID string, role domain.Role, reg domain.Registration, hash string) (domain.User, error) {
	u, err := scanUser(tx.QueryRow(ctx, `
		INSERT INTO users (tenant_id, email, password_hash, display_name, role)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5)
		RETURNING `+userColumns,
		tenantID, reg.Email, hash, reg.DisplayName, role))
	if dbkit.IsUniqueViolation(err) {
		return domain.User{}, ErrEmailTaken
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("insert user: %w", err)
	}
	return u, nil
}

// RegisterOAuth turns the active visitor id into a customer with email and
// no password. It returns a not_found error when id is not an active
// visitor.
func (Users) RegisterOAuth(ctx context.Context, tx pgx.Tx, id, email, displayName string) (domain.User, error) {
	u, err := scanUser(tx.QueryRow(ctx, `
		UPDATE users
		   SET email = $2, display_name = NULLIF($3, ''), role = $4, updated_at = NOW()
		 WHERE id = $1 AND role = $5 AND status = 'active'
		RETURNING `+userColumns,
		id, email, displayName, domain.RoleCustomer, domain.RoleVisitor))
	switch {
	case dbkit.IsNoRows(err):
		return domain.User{}, ycerrors.NotFound("Visitor", id)
	case dbkit.IsUniqueViolation(err):
		return domain.User{}, ErrEmailTaken
	case err != nil:
		return domain.User{}, fmt.Errorf("register visitor via oauth: %w", err)
	}
	return u, nil
}

// CreateOAuth inserts a customer with email and no password.
func (Users) CreateOAuth(ctx context.Context, tx pgx.Tx, tenantID, email, displayName string) (domain.User, error) {
	u, err := scanUser(tx.QueryRow(ctx, `
		INSERT INTO users (tenant_id, email, display_name, role)
		VALUES ($1, $2, NULLIF($3, ''), $4)
		RETURNING `+userColumns,
		tenantID, email, displayName, domain.RoleCustomer))
	if dbkit.IsUniqueViolation(err) {
		return domain.User{}, ErrEmailTaken
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("insert oauth user: %w", err)
	}
	return u, nil
}

// SetPassword replaces the password hash of user id.
func (Users) SetPassword(ctx context.Context, tx pgx.Tx, id, hash string) error {
	tag, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2, updated_at = NOW() WHERE id = $1`, id, hash)
	if err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ycerrors.NotFound("User", id)
	}
	return nil
}

// SetRole changes the role of user id.
func (Users) SetRole(ctx context.Context, tx pgx.Tx, id string, role domain.Role) (domain.User, error) {
	u, err := scanUser(tx.QueryRow(ctx,
		`UPDATE users SET role = $2, updated_at = NOW() WHERE id = $1 RETURNING `+userColumns, id, role))
	if dbkit.IsNoRows(err) {
		return domain.User{}, ycerrors.NotFound("User", id)
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("set role: %w", err)
	}
	return u, nil
}

// SetStatus changes the status of user id (active or suspended).
func (Users) SetStatus(ctx context.Context, tx pgx.Tx, id string, status domain.Status) (domain.User, error) {
	u, err := scanUser(tx.QueryRow(ctx,
		`UPDATE users SET status = $2, updated_at = NOW() WHERE id = $1 RETURNING `+userColumns, id, status))
	if dbkit.IsNoRows(err) {
		return domain.User{}, ycerrors.NotFound("User", id)
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("set status: %w", err)
	}
	return u, nil
}

// SetDisplayName replaces the display name of user id.
func (Users) SetDisplayName(ctx context.Context, tx pgx.Tx, id, displayName string) error {
	tag, err := tx.Exec(ctx,
		`UPDATE users SET display_name = NULLIF($2, ''), updated_at = NOW() WHERE id = $1`, id, displayName)
	if err != nil {
		return fmt.Errorf("set display name: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ycerrors.NotFound("User", id)
	}
	return nil
}

// ActivateAdmin upgrades an existing customer to role with an optional new
// display name, and sets status active (invitation accept, ADR-030 rule 6).
func (Users) ActivateAdmin(ctx context.Context, tx pgx.Tx, id string, role domain.Role, displayName string) (domain.User, error) {
	u, err := scanUser(tx.QueryRow(ctx, `
		UPDATE users
		   SET role = $2, status = $3,
		       display_name = CASE WHEN $4 = '' THEN display_name ELSE $4 END,
		       updated_at = NOW()
		 WHERE id = $1
		RETURNING `+userColumns, id, role, domain.StatusActive, displayName))
	if dbkit.IsNoRows(err) {
		return domain.User{}, ycerrors.NotFound("User", id)
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("activate admin: %w", err)
	}
	return u, nil
}

// CountActive returns how many users in tenantID have role and status active.
func (Users) CountActive(ctx context.Context, tx pgx.Tx, tenantID string, role domain.Role) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM users
		 WHERE tenant_id = $1 AND role = $2 AND status = $3`,
		tenantID, role, domain.StatusActive).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count active users: %w", err)
	}
	return n, nil
}

// Anonymise marks user id deleted and clears its personal data: email,
// phone, display name and password. The row stays so its ID is never
// reused and tokens naming it stop validating.
func (Users) Anonymise(ctx context.Context, tx pgx.Tx, id string, at time.Time) error {
	tag, err := tx.Exec(ctx, `UPDATE users SET email = NULL, phone_e164 = NULL, profile_image_url = NULL,
			display_name = NULL, password_hash = NULL, status = 'deleted', deleted_at = $2, updated_at = $2
		WHERE id = $1`, id, at)
	if err != nil {
		return fmt.Errorf("anonymise user: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ycerrors.NotFound("User", id)
	}
	return nil
}

// Pseudonyms is the pseudonym_map table (ADR-005).
type Pseudonyms struct{}

// Insert records user id's pseudonym.
func (Pseudonyms) Insert(ctx context.Context, tx pgx.Tx, tenantID, userID, pseudonym, requestedBy string, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO pseudonym_map (user_id, tenant_id, pseudonym, requested_by, created_at)
		VALUES ($1, $2, $3, $4, $5)`, userID, tenantID, pseudonym, requestedBy, at)
	if err != nil {
		return fmt.Errorf("insert pseudonym: %w", err)
	}
	return nil
}

// Filter narrows List. Empty fields match everything.
type Filter struct {
	TenantID string
	Email    string
	Role     domain.Role
}

// Cursor is the keyset position after the last user of a page.
type Cursor struct {
	CreatedAt time.Time `json:"c"`
	ID        string    `json:"i"`
}

// List returns up to limit users matching f, newest first, after cursor.
func (Users) List(ctx context.Context, tx pgx.Tx, f Filter, after *Cursor, limit int) ([]domain.User, error) {
	q := `SELECT ` + userColumns + ` FROM users
	       WHERE ($1 = '' OR tenant_id = $1)
	         AND ($2 = '' OR email = $2)
	         AND ($3 = '' OR role = $3)`
	args := []any{f.TenantID, f.Email, string(f.Role)}
	if after != nil {
		q += ` AND (created_at, id) < ($4, $5)`
		args = append(args, after.CreatedAt, after.ID)
	}
	q += fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT %d`, limit)

	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	users, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.User, error) { return scanUser(r) })
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return users, nil
}

func scanUser(row pgx.Row) (domain.User, error) {
	var u domain.User
	err := row.Scan(&u.ID, &u.TenantID, &u.Email, &u.DisplayName, &u.PhoneE164, &u.ProfileImage, &u.PasswordHash, &u.Role, &u.Status, &u.CreatedAt)
	return u, err
}

// ApplyOAuthProfile persists phone/image merge rules for an existing user.
func (Users) ApplyOAuthProfile(ctx context.Context, tx pgx.Tx, u domain.User) error {
	_, err := tx.Exec(ctx, `UPDATE users SET phone_e164 = NULLIF($2, ''), profile_image_url = NULLIF($3, ''),
		updated_at = NOW() WHERE id = $1`, u.ID, u.PhoneE164, u.ProfileImage)
	if err != nil {
		return fmt.Errorf("apply oauth profile: %w", err)
	}
	return nil
}

// UpdateProfile persists display name, phone and profile image for user id
// (issue #37). Empty strings are stored as NULL.
func (Users) UpdateProfile(ctx context.Context, tx pgx.Tx, u domain.User) (domain.User, error) {
	out, err := scanUser(tx.QueryRow(ctx, `
		UPDATE users
		   SET display_name = NULLIF($2, ''),
		       phone_e164 = NULLIF($3, ''),
		       profile_image_url = NULLIF($4, ''),
		       updated_at = NOW()
		 WHERE id = $1
		RETURNING `+userColumns, u.ID, u.DisplayName, u.PhoneE164, u.ProfileImage))
	if dbkit.IsNoRows(err) {
		return domain.User{}, ycerrors.NotFound("User", u.ID)
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("update profile: %w", err)
	}
	return out, nil
}
