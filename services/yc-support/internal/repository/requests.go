// Package repository is yc-support's Postgres access. Every method runs in
// the caller's transaction, so RLS (app.tenant_id) scopes the rows.
package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	"github.com/slaghuis/yc-platform/services/yc-support/internal/domain"
)

// ErrReferenceTaken is returned by Insert when the shop already has a
// request with that reference; the caller picks another.
var ErrReferenceTaken = fmt.Errorf("support request reference taken")

// Requests reads and writes support_requests.
type Requests struct{}

const requestColumns = `id::text, tenant_id, reference, topic, status, contact_email, contact_name,
	COALESCE(contact_phone, ''), COALESCE(user_id::text, ''), user_type, COALESCE(order_id::text, ''),
	COALESCE(page_url, ''), last_activity_at, closed_at, COALESCE(closed_reason, ''), redacted_at,
	created_at, updated_at`

func scanRequest(row pgx.Row) (domain.Request, error) {
	var r domain.Request
	err := row.Scan(&r.ID, &r.TenantID, &r.Reference, &r.Topic, &r.Status, &r.ContactEmail, &r.ContactName,
		&r.ContactPhone, &r.UserID, &r.UserType, &r.OrderID,
		&r.PageURL, &r.LastActivityAt, &r.ClosedAt, &r.ClosedReason, &r.RedactedAt,
		&r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// Insert stores a new request. It returns ErrReferenceTaken when the
// reference is in use in the shop.
func (Requests) Insert(ctx context.Context, tx pgx.Tx, r domain.Request) error {
	// A savepoint keeps the transaction usable after a unique violation.
	sp, err := tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("insert support request: %w", err)
	}
	_, err = sp.Exec(ctx, `
		INSERT INTO support_requests (tenant_id, id, reference, topic, status, contact_email, contact_name,
			contact_phone, user_id, user_type, order_id, page_url, last_activity_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), NULLIF($9, '')::uuid, $10, NULLIF($11, '')::uuid,
			NULLIF($12, ''), $13, $14, $14)`,
		r.TenantID, r.ID, r.Reference, string(r.Topic), string(r.Status), r.ContactEmail, r.ContactName,
		r.ContactPhone, r.UserID, string(r.UserType), r.OrderID, r.PageURL, r.LastActivityAt, r.CreatedAt)
	if err != nil {
		_ = sp.Rollback(ctx)
		if dbkit.IsUniqueViolation(err) {
			return ErrReferenceTaken
		}
		return fmt.Errorf("insert support request: %w", err)
	}
	if err := sp.Commit(ctx); err != nil {
		return fmt.Errorf("insert support request: %w", err)
	}
	return nil
}

// Get returns a request, locked when forUpdate, or false when the shop has
// none with that ID.
func (Requests) Get(ctx context.Context, tx pgx.Tx, tenantID, id string, forUpdate bool) (domain.Request, bool, error) {
	q := `SELECT ` + requestColumns + ` FROM support_requests WHERE tenant_id = $1 AND id = $2`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	r, err := scanRequest(tx.QueryRow(ctx, q, tenantID, id))
	if dbkit.IsNoRows(err) {
		return domain.Request{}, false, nil
	}
	if err != nil {
		return domain.Request{}, false, fmt.Errorf("get support request: %w", err)
	}
	return r, true, nil
}

// Cursor is the position after the last row of a page.
type Cursor struct {
	At time.Time `json:"a"`
	ID string    `json:"i"`
}

// Filter selects requests. Empty fields match any.
type Filter struct {
	UserID    string
	Status    domain.Status
	Topic     domain.Topic
	Email     string
	Reference string
}

// List returns up to limit requests matching f, most recently active
// first, after the cursor.
func (Requests) List(ctx context.Context, tx pgx.Tx, tenantID string, f Filter, after *Cursor, limit int) ([]domain.Request, error) {
	q := `SELECT ` + requestColumns + ` FROM support_requests WHERE tenant_id = $1`
	args := []any{tenantID}
	add := func(cond string, v any) {
		args = append(args, v)
		q += fmt.Sprintf(" AND "+cond, len(args))
	}
	if f.UserID != "" {
		add("user_id = $%d", f.UserID)
	}
	if f.Status != "" {
		add("status = $%d", string(f.Status))
	}
	if f.Topic != "" {
		add("topic = $%d", string(f.Topic))
	}
	if f.Email != "" {
		add("contact_email = $%d", f.Email)
	}
	if f.Reference != "" {
		add("reference = $%d", f.Reference)
	}
	if after != nil {
		args = append(args, after.At, after.ID)
		q += fmt.Sprintf(` AND (last_activity_at, id) < ($%d, $%d::uuid)`, len(args)-1, len(args))
	}
	args = append(args, limit)
	q += fmt.Sprintf(` ORDER BY last_activity_at DESC, id DESC LIMIT $%d`, len(args))
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list support requests: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Request, error) { return scanRequest(r) })
	if err != nil {
		return nil, fmt.Errorf("list support requests: %w", err)
	}
	return out, nil
}

// Update writes the state a post or a close changes.
func (Requests) Update(ctx context.Context, tx pgx.Tx, r domain.Request) error {
	_, err := tx.Exec(ctx, `UPDATE support_requests SET status = $3, last_activity_at = $4, closed_at = $5,
		closed_reason = NULLIF($6, ''), updated_at = $7 WHERE tenant_id = $1 AND id = $2`,
		r.TenantID, r.ID, string(r.Status), r.LastActivityAt, r.ClosedAt, string(r.ClosedReason), r.UpdatedAt)
	if err != nil {
		return fmt.Errorf("update support request: %w", err)
	}
	return nil
}

// Claim attaches the shop's unclaimed requests from email to userID and
// returns how many it changed. Requests the customer already owns, and
// ones erased by GDPR, are left alone.
func (Requests) Claim(ctx context.Context, tx pgx.Tx, tenantID, userID, email string, now time.Time) (int64, error) {
	tag, err := tx.Exec(ctx, `UPDATE support_requests SET user_id = $2, user_type = 'customer', updated_at = $4
		WHERE tenant_id = $1 AND user_id IS NULL AND contact_email = $3 AND redacted_at IS NULL`,
		tenantID, userID, email, now)
	if err != nil {
		return 0, fmt.Errorf("claim support requests: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Redact erases userID's contact details (GDPR) and detaches the user. It
// returns the IDs of the requests it changed.
func (Requests) Redact(ctx context.Context, tx pgx.Tx, tenantID, userID string, now time.Time) ([]string, error) {
	rows, err := tx.Query(ctx, `UPDATE support_requests SET contact_email = '', contact_name = '',
		contact_phone = NULL, page_url = NULL, user_id = NULL, redacted_at = $3, updated_at = $3
		WHERE tenant_id = $1 AND user_id = $2 RETURNING id::text`, tenantID, userID, now)
	if err != nil {
		return nil, fmt.Errorf("redact support requests: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("redact support requests: %w", err)
	}
	return ids, nil
}

// Due is a request that may be idle, with its shop.
type Due struct {
	TenantID string
	ID       string
}

// DueIdle returns open requests with no activity since before, across
// shops, oldest first. Call it in a platform transaction.
func (Requests) DueIdle(ctx context.Context, tx pgx.Tx, before time.Time, limit int) ([]Due, error) {
	rows, err := tx.Query(ctx, `SELECT tenant_id, id::text FROM support_requests
		WHERE status <> 'closed' AND last_activity_at <= $1 ORDER BY last_activity_at LIMIT $2`, before, limit)
	if err != nil {
		return nil, fmt.Errorf("due support requests: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Due, error) {
		var d Due
		err := r.Scan(&d.TenantID, &d.ID)
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("due support requests: %w", err)
	}
	return out, nil
}

// LockIdle locks a request that is still open and idle, skipping one
// another sweeper holds.
func (Requests) LockIdle(ctx context.Context, tx pgx.Tx, tenantID, id string, before time.Time) (domain.Request, bool, error) {
	r, err := scanRequest(tx.QueryRow(ctx, `SELECT `+requestColumns+` FROM support_requests
		WHERE tenant_id = $1 AND id = $2 AND status <> 'closed' AND last_activity_at <= $3
		FOR UPDATE SKIP LOCKED`, tenantID, id, before))
	if dbkit.IsNoRows(err) {
		return domain.Request{}, false, nil
	}
	if err != nil {
		return domain.Request{}, false, fmt.Errorf("lock idle support request: %w", err)
	}
	return r, true, nil
}
