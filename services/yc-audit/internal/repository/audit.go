// Package repository is yc-audit's Postgres access. Every function runs in
// the caller's transaction, under its row-level security context.
package repository

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	"github.com/slaghuis/yc-platform/services/yc-audit/internal/domain"
)

// Audit is the repository. It is stateless.
type Audit struct{}

// InsertRecord stores a record; false means the event was recorded before.
func (Audit) InsertRecord(ctx context.Context, tx pgx.Tx, r domain.Record) (bool, error) {
	tag, err := tx.Exec(ctx, `INSERT INTO audit_records (id, tenant_id, category, event_type, event_id, source_service,
			actor_type, actor_id, subject_ref, summary, correlation_id, payload, event_time, recorded_at)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), NULLIF($8, ''), NULLIF($9, ''), $10, NULLIF($11, ''), $12, $13, $14)
		ON CONFLICT (event_id) DO NOTHING`,
		r.ID, r.TenantID, string(r.Category), r.EventType, r.EventID, r.SourceService, r.ActorType, r.ActorID,
		r.SubjectRef, r.Summary, r.CorrelationID, r.Payload, r.EventTime, r.RecordedAt)
	if err != nil {
		return false, fmt.Errorf("insert audit record: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// RecordFilter narrows a listing. Zero values match everything.
type RecordFilter struct {
	TenantID   string
	Categories []domain.Category
	EventType  string
	SubjectRef string
	From, To   *time.Time
}

// Cursor is the position after the last record of a page.
type Cursor struct {
	EventTime time.Time `json:"t"`
	ID        string    `json:"id"`
}

const recordColumns = `id, tenant_id, category, event_type, event_id, source_service, COALESCE(actor_type, ''),
	COALESCE(actor_id, ''), COALESCE(subject_ref, ''), summary, COALESCE(correlation_id, ''), payload, event_time, recorded_at`

func scanRecord(r pgx.Row) (domain.Record, error) {
	var rec domain.Record
	var category string
	err := r.Scan(&rec.ID, &rec.TenantID, &category, &rec.EventType, &rec.EventID, &rec.SourceService, &rec.ActorType,
		&rec.ActorID, &rec.SubjectRef, &rec.Summary, &rec.CorrelationID, &rec.Payload, &rec.EventTime, &rec.RecordedAt)
	rec.Category = domain.Category(category)
	return rec, err
}

// ListRecords returns records newest first.
func (Audit) ListRecords(ctx context.Context, tx pgx.Tx, f RecordFilter, after *Cursor, limit int) ([]domain.Record, error) {
	var where []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if f.TenantID != "" {
		where = append(where, "tenant_id = "+arg(f.TenantID))
	}
	if len(f.Categories) > 0 {
		cats := make([]string, len(f.Categories))
		for i, c := range f.Categories {
			cats[i] = string(c)
		}
		where = append(where, "category = ANY("+arg(cats)+")")
	}
	if f.EventType != "" {
		where = append(where, "event_type = "+arg(f.EventType))
	}
	if f.SubjectRef != "" {
		where = append(where, "subject_ref = "+arg(f.SubjectRef))
	}
	if f.From != nil {
		where = append(where, "event_time >= "+arg(*f.From))
	}
	if f.To != nil {
		where = append(where, "event_time < "+arg(*f.To))
	}
	if after != nil {
		where = append(where, "(event_time, id) < ("+arg(after.EventTime)+", "+arg(after.ID)+"::uuid)")
	}
	q := `SELECT ` + recordColumns + ` FROM audit_records`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY event_time DESC, id DESC LIMIT " + arg(limit)
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list audit records: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Record, error) { return scanRecord(r) })
	if err != nil {
		return nil, fmt.Errorf("list audit records: %w", err)
	}
	return out, nil
}

// Record returns one record, or false.
func (Audit) Record(ctx context.Context, tx pgx.Tx, id string) (domain.Record, bool, error) {
	rec, err := scanRecord(tx.QueryRow(ctx, `SELECT `+recordColumns+` FROM audit_records WHERE id = $1`, id))
	if dbkit.IsNoRows(err) {
		return rec, false, nil
	}
	if err != nil {
		return rec, false, fmt.Errorf("get audit record: %w", err)
	}
	return rec, true, nil
}

// OpenRegister starts a deletion's register entry; false means it exists.
func (Audit) OpenRegister(ctx context.Context, tx pgx.Tx, r domain.Register) (bool, error) {
	tag, err := tx.Exec(ctx, `INSERT INTO gdpr_cascade_register (tenant_id, user_id, pseudonym, role, requested_by,
			initiated_at, deadline, expected_services, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'in_progress') ON CONFLICT DO NOTHING`,
		r.TenantID, r.UserID, r.Pseudonym, r.Role, r.RequestedBy, r.InitiatedAt, r.Deadline, r.Expected)
	if err != nil {
		return false, fmt.Errorf("open register: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// AddReport stores a service's report; false means it reported before.
func (Audit) AddReport(ctx context.Context, tx pgx.Tx, tenantID, userID string, r domain.Report) (bool, error) {
	tag, err := tx.Exec(ctx, `INSERT INTO gdpr_cascade_reports (tenant_id, user_id, service, action, record_count,
			completed_at, reported_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT DO NOTHING`,
		tenantID, userID, r.Service, r.Action, r.RecordCount, r.CompletedAt, r.ReportedAt)
	if err != nil {
		return false, fmt.Errorf("add report: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

const registerColumns = `tenant_id, user_id, pseudonym, role, requested_by, initiated_at, deadline, expected_services,
	status, completed_at, incident_at`

func scanRegister(r pgx.Row) (domain.Register, error) {
	var reg domain.Register
	var status string
	err := r.Scan(&reg.TenantID, &reg.UserID, &reg.Pseudonym, &reg.Role, &reg.RequestedBy, &reg.InitiatedAt,
		&reg.Deadline, &reg.Expected, &status, &reg.CompletedAt, &reg.IncidentAt)
	reg.Status = domain.RegisterStatus(status)
	return reg, err
}

// Register returns a deletion with its reports, locked when forUpdate, or
// false. An empty tenantID looks in every tenant (platform context).
func (a Audit) Register(ctx context.Context, tx pgx.Tx, tenantID, userID string, forUpdate bool) (domain.Register, bool, error) {
	q := `SELECT ` + registerColumns + ` FROM gdpr_cascade_register WHERE user_id = $1 AND ($2 = '' OR tenant_id = $2)
		ORDER BY initiated_at DESC LIMIT 1`
	if forUpdate {
		q += " FOR UPDATE"
	}
	reg, err := scanRegister(tx.QueryRow(ctx, q, userID, tenantID))
	if dbkit.IsNoRows(err) {
		return reg, false, nil
	}
	if err != nil {
		return reg, false, fmt.Errorf("get register: %w", err)
	}
	reg.Reports, err = a.reports(ctx, tx, reg.TenantID, reg.UserID)
	return reg, err == nil, err
}

func (Audit) reports(ctx context.Context, tx pgx.Tx, tenantID, userID string) ([]domain.Report, error) {
	rows, err := tx.Query(ctx, `SELECT service, action, record_count, completed_at, reported_at FROM gdpr_cascade_reports
		WHERE tenant_id = $1 AND user_id = $2 ORDER BY reported_at, service`, tenantID, userID)
	if err != nil {
		return nil, fmt.Errorf("list reports: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Report, error) {
		var rep domain.Report
		err := r.Scan(&rep.Service, &rep.Action, &rep.RecordCount, &rep.CompletedAt, &rep.ReportedAt)
		return rep, err
	})
	if err != nil {
		return nil, fmt.Errorf("list reports: %w", err)
	}
	return out, nil
}

// SetRegisterStatus moves a deletion to complete or incident.
func (Audit) SetRegisterStatus(ctx context.Context, tx pgx.Tx, r domain.Register) error {
	_, err := tx.Exec(ctx, `UPDATE gdpr_cascade_register SET status = $3, completed_at = $4, incident_at = $5
		WHERE tenant_id = $1 AND user_id = $2`, r.TenantID, r.UserID, string(r.Status), r.CompletedAt, r.IncidentAt)
	if err != nil {
		return fmt.Errorf("update register: %w", err)
	}
	return nil
}

// RegisterKey identifies a deletion.
type RegisterKey struct {
	TenantID string
	UserID   string
}

// Overdue returns in-progress deletions past their deadline.
func (Audit) Overdue(ctx context.Context, tx pgx.Tx, now time.Time, limit int) ([]RegisterKey, error) {
	rows, err := tx.Query(ctx, `SELECT tenant_id, user_id FROM gdpr_cascade_register
		WHERE status = 'in_progress' AND deadline < $1 ORDER BY deadline LIMIT $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("overdue registers: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[RegisterKey])
	if err != nil {
		return nil, fmt.Errorf("overdue registers: %w", err)
	}
	return out, nil
}

// RegisterFilter narrows the register listing.
type RegisterFilter struct {
	Status domain.RegisterStatus
	// Incidents lists deletions that ever breached their deadline.
	Incidents bool
}

// RegisterCursor is the position after the last entry of a page.
type RegisterCursor struct {
	InitiatedAt time.Time `json:"t"`
	UserID      string    `json:"u"`
}

// ListRegister returns deletions newest first, without their reports.
func (Audit) ListRegister(ctx context.Context, tx pgx.Tx, f RegisterFilter, after *RegisterCursor, limit int) ([]domain.Register, error) {
	var at *time.Time
	var user string
	if after != nil {
		at, user = &after.InitiatedAt, after.UserID
	}
	rows, err := tx.Query(ctx, `SELECT `+registerColumns+` FROM gdpr_cascade_register
		WHERE ($1 = '' OR status = $1) AND (NOT $2 OR incident_at IS NOT NULL)
			AND ($3::timestamptz IS NULL OR (initiated_at, user_id) < ($3, $4))
		ORDER BY initiated_at DESC, user_id DESC LIMIT $5`, string(f.Status), f.Incidents, at, user, limit)
	if err != nil {
		return nil, fmt.Errorf("list register: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Register, error) { return scanRegister(r) })
	if err != nil {
		return nil, fmt.Errorf("list register: %w", err)
	}
	return out, nil
}
