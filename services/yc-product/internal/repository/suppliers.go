// Package repository is yc-product's Postgres access. Every method runs in
// the caller's transaction, so RLS (app.tenant_id) scopes it.
package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	"github.com/slaghuis/yc-platform/services/yc-product/internal/domain"
)

// Catalog reads and writes the catalog tables.
type Catalog struct{}

// Cursor is a keyset position: newest first by created_at, then id.
type Cursor struct {
	CreatedAt time.Time `json:"c"`
	ID        string    `json:"i"`
	// Sort and Key position a browse cursor under a sort other than
	// newest: Key is the popularity or price key of the last product.
	Sort string `json:"s,omitempty"`
	Key  int64  `json:"k,omitempty"`
}

const supplierColumns = `id, tenant_id, name, contact, status, created_at, updated_at`

func scanSupplier(row pgx.Row) (domain.Supplier, error) {
	var s domain.Supplier
	err := row.Scan(&s.ID, &s.TenantID, &s.Name, &s.Contact, &s.Status, &s.CreatedAt, &s.UpdatedAt)
	return s, err
}

// CreateSupplier inserts a supplier for tenantID.
func (Catalog) CreateSupplier(ctx context.Context, tx pgx.Tx, tenantID string, in domain.SupplierInput) (domain.Supplier, error) {
	s, err := scanSupplier(tx.QueryRow(ctx, `
		INSERT INTO suppliers (tenant_id, name, contact, status) VALUES ($1, $2, $3, $4)
		RETURNING `+supplierColumns, tenantID, in.Name, in.Contact, in.Status))
	if err != nil {
		return domain.Supplier{}, fmt.Errorf("create supplier: %w", err)
	}
	return s, nil
}

// GetSupplier returns a supplier visible to the transaction, locked when
// forUpdate.
func (Catalog) GetSupplier(ctx context.Context, tx pgx.Tx, id string, forUpdate bool) (domain.Supplier, error) {
	q := `SELECT ` + supplierColumns + ` FROM suppliers WHERE id = $1`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	s, err := scanSupplier(tx.QueryRow(ctx, q, id))
	if dbkit.IsNoRows(err) {
		return domain.Supplier{}, ycerrors.NotFound("Supplier", id)
	}
	if err != nil {
		return domain.Supplier{}, fmt.Errorf("get supplier: %w", err)
	}
	return s, nil
}

// UpdateSupplier replaces a supplier's settable fields.
func (Catalog) UpdateSupplier(ctx context.Context, tx pgx.Tx, id string, in domain.SupplierInput) (domain.Supplier, error) {
	s, err := scanSupplier(tx.QueryRow(ctx, `
		UPDATE suppliers SET name = $2, contact = $3, status = $4, updated_at = NOW()
		WHERE id = $1 RETURNING `+supplierColumns, id, in.Name, in.Contact, in.Status))
	if dbkit.IsNoRows(err) {
		return domain.Supplier{}, ycerrors.NotFound("Supplier", id)
	}
	if err != nil {
		return domain.Supplier{}, fmt.Errorf("update supplier: %w", err)
	}
	return s, nil
}

// DeleteSupplier deletes a supplier no product references.
func (Catalog) DeleteSupplier(ctx context.Context, tx pgx.Tx, id string) error {
	tag, err := tx.Exec(ctx, `DELETE FROM suppliers WHERE id = $1`, id)
	if dbkit.IsForeignKeyViolation(err) {
		return ycerrors.Conflict("Products still reference this supplier")
	}
	if err != nil {
		return fmt.Errorf("delete supplier: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ycerrors.NotFound("Supplier", id)
	}
	return nil
}

// ListSuppliers returns up to limit suppliers after the cursor, newest
// first, of tenantID or every tenant the transaction sees when "".
func (Catalog) ListSuppliers(ctx context.Context, tx pgx.Tx, tenantID string, after *Cursor, limit int) ([]domain.Supplier, error) {
	q := `SELECT ` + supplierColumns + ` FROM suppliers WHERE ($1 = '' OR tenant_id = $1)`
	args := []any{tenantID}
	if after != nil {
		q += ` AND (created_at, id) < ($2, $3)`
		args = append(args, after.CreatedAt, after.ID)
	}
	q += fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT %d`, limit)
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list suppliers: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Supplier, error) { return scanSupplier(r) })
	if err != nil {
		return nil, fmt.Errorf("list suppliers: %w", err)
	}
	return out, nil
}

// ActiveSuppliers returns the transaction tenant's active suppliers by
// name.
func (Catalog) ActiveSuppliers(ctx context.Context, tx pgx.Tx) ([]domain.Supplier, error) {
	rows, err := tx.Query(ctx, `SELECT `+supplierColumns+` FROM suppliers WHERE status = 'active' ORDER BY name, id`)
	if err != nil {
		return nil, fmt.Errorf("active suppliers: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Supplier, error) { return scanSupplier(r) })
	if err != nil {
		return nil, fmt.Errorf("active suppliers: %w", err)
	}
	return out, nil
}
