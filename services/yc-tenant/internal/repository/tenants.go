// Package repository is yc-tenant's data access. The tables are
// platform-wide, so callers run it inside dbkit.WithPlatformTx.
package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	"github.com/slaghuis/yc-platform/services/yc-tenant/internal/domain"
)

// ErrConflict means a unique constraint refused the write: the tenant ID or
// a hostname is taken.
var ErrConflict = errors.New("repository: tenant id or hostname already exists")

// Cursor is the position after a listed tenant (listings are ordered by
// tenant_id).
type Cursor struct {
	TenantID string `json:"tenant_id"`
}

// Tenants stores tenants and their hostnames.
type Tenants struct{}

const tenantColumns = `tenant_id, name, status, store_currency, default_jurisdiction, is_tax_inclusive,
	contact_email, COALESCE(vat_number, ''), invoice_address, created_at, updated_at, suspended_at,
	COALESCE(suspended_reason, ''), deleted_at, data_purged_at`

func scanTenant(row pgx.Row) (domain.Tenant, error) {
	var t domain.Tenant
	var status string
	var invoice []byte
	err := row.Scan(&t.TenantID, &t.Name, &status, &t.StoreCurrency, &t.DefaultJurisdiction, &t.IsTaxInclusive,
		&t.ContactEmail, &t.VATNumber, &invoice, &t.CreatedAt, &t.UpdatedAt, &t.SuspendedAt, &t.SuspendedReason,
		&t.DeletedAt, &t.DataPurgedAt)
	t.Status = domain.Status(status)
	if err != nil {
		return t, err
	}
	if len(invoice) > 0 {
		var a domain.InvoiceAddress
		if err := json.Unmarshal(invoice, &a); err != nil {
			return t, fmt.Errorf("decode invoice_address: %w", err)
		}
		if !a.Empty() {
			t.InvoiceAddress = &a
		}
	}
	return t, nil
}

// Insert stores t and its hostnames. It returns ErrConflict when the
// tenant ID or a hostname exists.
func (Tenants) Insert(ctx context.Context, tx pgx.Tx, t domain.Tenant) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO tenants (tenant_id, name, status, store_currency, default_jurisdiction, is_tax_inclusive,
			contact_email, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		t.TenantID, t.Name, string(t.Status), t.StoreCurrency, t.DefaultJurisdiction, t.IsTaxInclusive,
		t.ContactEmail, t.CreatedAt, t.UpdatedAt)
	if err != nil {
		return conflictOr(err, "insert tenant")
	}
	return insertHostnames(ctx, tx, t.TenantID, t.Hostnames)
}

func insertHostnames(ctx context.Context, tx pgx.Tx, tenantID string, hs []domain.Hostname) error {
	for _, h := range hs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO tenant_hostnames (hostname, tenant_id, is_primary) VALUES ($1, $2, $3)`,
			h.Name, tenantID, h.IsPrimary); err != nil {
			return conflictOr(err, "insert hostname")
		}
	}
	return nil
}

// Get returns a tenant with its hostnames. With lock the row is held for
// the rest of the transaction.
func (Tenants) Get(ctx context.Context, tx pgx.Tx, tenantID string, lock bool) (domain.Tenant, bool, error) {
	q := `SELECT ` + tenantColumns + ` FROM tenants WHERE tenant_id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	t, err := scanTenant(tx.QueryRow(ctx, q, tenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Tenant{}, false, nil
	}
	if err != nil {
		return domain.Tenant{}, false, fmt.Errorf("get tenant: %w", err)
	}
	hosts, err := hostnamesOf(ctx, tx, []string{tenantID})
	if err != nil {
		return domain.Tenant{}, false, err
	}
	t.Hostnames = hosts[tenantID]
	return t, true, nil
}

// List returns up to limit tenants after the cursor, ordered by tenant_id,
// each with its hostnames.
func (Tenants) List(ctx context.Context, tx pgx.Tx, f domain.ListFilter, after *Cursor, limit int) ([]domain.Tenant, error) {
	afterID := ""
	if after != nil {
		afterID = after.TenantID
	}
	pattern := ""
	if f.Search != "" {
		pattern = "%" + likeEscaper.Replace(f.Search) + "%"
	}
	rows, err := tx.Query(ctx, `
		SELECT `+tenantColumns+` FROM tenants t
		WHERE ($1 = '' OR t.status = $1)
		  AND ($2 = '' OR t.tenant_id > $2)
		  AND ($3 = '' OR t.name ILIKE $3 ESCAPE '\'
		       OR EXISTS (SELECT 1 FROM tenant_hostnames h WHERE h.tenant_id = t.tenant_id AND h.hostname ILIKE $3 ESCAPE '\'))
		ORDER BY t.tenant_id
		LIMIT $4`, string(f.Status), afterID, pattern, limit)
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Tenant, error) { return scanTenant(r) })
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	ids := make([]string, len(out))
	for i, t := range out {
		ids[i] = t.TenantID
	}
	hosts, err := hostnamesOf(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Hostnames = hosts[out[i].TenantID]
	}
	return out, nil
}

// likeEscaper escapes the LIKE wildcards in a search term.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// hostnamesOf returns each tenant's hostnames, the primary first.
func hostnamesOf(ctx context.Context, tx pgx.Tx, tenantIDs []string) (map[string][]domain.Hostname, error) {
	out := make(map[string][]domain.Hostname, len(tenantIDs))
	if len(tenantIDs) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT tenant_id, hostname, is_primary FROM tenant_hostnames
		WHERE tenant_id = ANY($1) ORDER BY tenant_id, is_primary DESC, hostname`, tenantIDs)
	if err != nil {
		return nil, fmt.Errorf("list hostnames: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var h domain.Hostname
		if err := rows.Scan(&id, &h.Name, &h.IsPrimary); err != nil {
			return nil, fmt.Errorf("list hostnames: %w", err)
		}
		out[id] = append(out[id], h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list hostnames: %w", err)
	}
	return out, nil
}

// HostnameOwners returns the tenant holding each of hostnames; hostnames
// nobody holds are left out.
func (Tenants) HostnameOwners(ctx context.Context, tx pgx.Tx, hostnames []string) (map[string]string, error) {
	rows, err := tx.Query(ctx, `SELECT hostname, tenant_id FROM tenant_hostnames WHERE hostname = ANY($1)`, hostnames)
	if err != nil {
		return nil, fmt.Errorf("hostname owners: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var h, id string
		if err := rows.Scan(&h, &id); err != nil {
			return nil, fmt.Errorf("hostname owners: %w", err)
		}
		out[h] = id
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("hostname owners: %w", err)
	}
	return out, nil
}

// UpdateDetails sets the name, contact email and invoice identity.
func (Tenants) UpdateDetails(ctx context.Context, tx pgx.Tx, t domain.Tenant) error {
	var invoice []byte
	var err error
	if t.InvoiceAddress != nil && !t.InvoiceAddress.Empty() {
		if invoice, err = json.Marshal(t.InvoiceAddress); err != nil {
			return fmt.Errorf("encode invoice_address: %w", err)
		}
	}
	_, err = tx.Exec(ctx, `
		UPDATE tenants SET name = $2, contact_email = $3, vat_number = $4, invoice_address = $5, updated_at = $6
		WHERE tenant_id = $1`,
		t.TenantID, t.Name, t.ContactEmail, t.VATNumber, invoice, t.UpdatedAt)
	if err != nil {
		return fmt.Errorf("update tenant: %w", err)
	}
	return nil
}

// ReplaceHostnames replaces a tenant's hostnames. It returns ErrConflict
// when another tenant holds one.
func (Tenants) ReplaceHostnames(ctx context.Context, tx pgx.Tx, tenantID string, hs []domain.Hostname, at time.Time) error {
	if _, err := tx.Exec(ctx, `DELETE FROM tenant_hostnames WHERE tenant_id = $1`, tenantID); err != nil {
		return fmt.Errorf("replace hostnames: %w", err)
	}
	if err := insertHostnames(ctx, tx, tenantID, hs); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE tenants SET updated_at = $2 WHERE tenant_id = $1`, tenantID, at); err != nil {
		return fmt.Errorf("replace hostnames: %w", err)
	}
	return nil
}

// SetStatus stores a lifecycle change: the status and its timestamps. A
// deleted tenant's hostnames are released.
func (Tenants) SetStatus(ctx context.Context, tx pgx.Tx, t domain.Tenant) error {
	_, err := tx.Exec(ctx, `
		UPDATE tenants SET status = $2, updated_at = $3, suspended_at = $4, suspended_reason = NULLIF($5, ''), deleted_at = $6
		WHERE tenant_id = $1`,
		t.TenantID, string(t.Status), t.UpdatedAt, t.SuspendedAt, t.SuspendedReason, t.DeletedAt)
	if err != nil {
		return fmt.Errorf("set tenant status: %w", err)
	}
	if t.Status == domain.StatusDeleted {
		if _, err := tx.Exec(ctx, `DELETE FROM tenant_hostnames WHERE tenant_id = $1`, t.TenantID); err != nil {
			return fmt.Errorf("release hostnames: %w", err)
		}
	}
	return nil
}

func conflictOr(err error, what string) error {
	if dbkit.IsUniqueViolation(err) {
		return fmt.Errorf("%s: %w", what, ErrConflict)
	}
	return fmt.Errorf("%s: %w", what, err)
}
