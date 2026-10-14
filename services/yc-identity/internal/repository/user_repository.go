// Package repository persists yc-identity's aggregates. Methods take the
// caller's transaction, whose RLS tenant context scopes every query.
package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/domain"
)

// Users reads and writes the users table.
type Users struct{}

const userColumns = "id, tenant_id, role, status, created_at"

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

func scanUser(row pgx.Row) (domain.User, error) {
	var u domain.User
	err := row.Scan(&u.ID, &u.TenantID, &u.Role, &u.Status, &u.CreatedAt)
	return u, err
}
