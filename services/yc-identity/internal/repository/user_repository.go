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

const userColumns = "id, tenant_id, COALESCE(email, ''), COALESCE(display_name, ''), COALESCE(password_hash, ''), role, status, created_at"

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
	err := row.Scan(&u.ID, &u.TenantID, &u.Email, &u.DisplayName, &u.PasswordHash, &u.Role, &u.Status, &u.CreatedAt)
	return u, err
}
