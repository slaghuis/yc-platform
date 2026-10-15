package service

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	"github.com/slaghuis/yc-platform/services/yc-audit/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-audit/internal/repository"
)

// Page is a page of results and the cursor after it.
type Page[T, C any] struct {
	Items []T
	Next  *C
}

// Records lists any shop's records for a system admin.
func (s *Service) Records(ctx context.Context, f repository.RecordFilter, after *repository.Cursor, limit int) (Page[domain.Record, repository.Cursor], error) {
	if err := requireSystemAdmin(ctx); err != nil {
		return Page[domain.Record, repository.Cursor]{}, err
	}
	if err := validCategories(f.Categories); err != nil {
		return Page[domain.Record, repository.Cursor]{}, err
	}
	return s.records(ctx, dbkit.WithPlatformTx, f, after, limit)
}

// ShopRecords lists the calling shop admin's shop's records, without the
// GDPR ones.
func (s *Service) ShopRecords(ctx context.Context, f repository.RecordFilter, after *repository.Cursor, limit int) (Page[domain.Record, repository.Cursor], error) {
	tenantID, err := shopAdminTenant(ctx)
	if err != nil {
		return Page[domain.Record, repository.Cursor]{}, err
	}
	if err := validCategories(f.Categories); err != nil {
		return Page[domain.Record, repository.Cursor]{}, err
	}
	if len(f.Categories) == 0 {
		f.Categories = domain.ShopCategories()
	}
	for _, c := range f.Categories {
		if !c.ShopVisible() {
			return Page[domain.Record, repository.Cursor]{}, ycerrors.Forbidden(string(c) + " records are for system admins")
		}
	}
	f.TenantID = tenantID
	inShop := func(ctx context.Context, db *pgxpool.Pool, fn dbkit.TxFunc) error {
		return dbkit.WithTenantTx(ctx, db, tenantID, fn)
	}
	return s.records(ctx, inShop, f, after, limit)
}

func (s *Service) records(ctx context.Context, in func(context.Context, *pgxpool.Pool, dbkit.TxFunc) error,
	f repository.RecordFilter, after *repository.Cursor, limit int,
) (Page[domain.Record, repository.Cursor], error) {
	var page Page[domain.Record, repository.Cursor]
	err := in(ctx, s.db, func(tx pgx.Tx) error {
		items, err := s.repo.ListRecords(ctx, tx, f, after, limit+1)
		if err != nil {
			return err
		}
		if len(items) > limit {
			items = items[:limit]
			last := items[limit-1]
			page.Next = &repository.Cursor{EventTime: last.EventTime, ID: last.ID}
		}
		page.Items = items
		return nil
	})
	return page, err
}

// RecordByID returns one record for a system admin.
func (s *Service) RecordByID(ctx context.Context, id string) (domain.Record, error) {
	if err := requireSystemAdmin(ctx); err != nil {
		return domain.Record{}, err
	}
	var rec domain.Record
	err := dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		var ok bool
		var err error
		rec, ok, err = s.repo.Record(ctx, tx, id)
		if err == nil && !ok {
			err = ycerrors.NotFound("audit record", id)
		}
		return err
	})
	return rec, err
}

// Export formats.
const (
	FormatCSV   = "csv"
	FormatJSONL = "jsonl"
)

// Export returns up to the configured maximum of records, newest first, as
// CSV (without payloads) or JSON Lines (with them), and whether there were
// more.
func (s *Service) Export(ctx context.Context, f repository.RecordFilter, format string) ([]byte, bool, error) {
	if err := requireSystemAdmin(ctx); err != nil {
		return nil, false, err
	}
	if err := validCategories(f.Categories); err != nil {
		return nil, false, err
	}
	var recs []domain.Record
	err := dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		recs, err = s.repo.ListRecords(ctx, tx, f, nil, s.cfg.MaxExport+1)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	truncated := len(recs) > s.cfg.MaxExport
	if truncated {
		recs = recs[:s.cfg.MaxExport]
	}
	var buf bytes.Buffer
	switch format {
	case FormatJSONL:
		enc := json.NewEncoder(&buf)
		for _, r := range recs {
			if err := enc.Encode(exportRecord(r)); err != nil {
				return nil, false, wrapf("export: %w", err)
			}
		}
	case FormatCSV:
		w := csv.NewWriter(&buf)
		_ = w.Write([]string{
			"id", "event_time", "recorded_at", "tenant_id", "category", "event_type", "event_id",
			"source_service", "actor_type", "actor_id", "subject_ref", "summary", "correlation_id",
		})
		for _, r := range recs {
			_ = w.Write([]string{
				r.ID, r.EventTime.UTC().Format(time.RFC3339Nano), r.RecordedAt.UTC().Format(time.RFC3339Nano),
				r.TenantID, string(r.Category), r.EventType, r.EventID, r.SourceService, r.ActorType, r.ActorID,
				r.SubjectRef, r.Summary, r.CorrelationID,
			})
		}
		w.Flush()
		if err := w.Error(); err != nil {
			return nil, false, wrapf("export: %w", err)
		}
	default:
		return nil, false, ycerrors.Validation().Field("format", "invalid", "Format is csv or jsonl").Build()
	}
	return buf.Bytes(), truncated, nil
}

func exportRecord(r domain.Record) map[string]any {
	return map[string]any{
		"id": r.ID, "event_time": r.EventTime.UTC(), "recorded_at": r.RecordedAt.UTC(), "tenant_id": r.TenantID,
		"category": r.Category, "event_type": r.EventType, "event_id": r.EventID, "source_service": r.SourceService,
		"actor_type": r.ActorType, "actor_id": r.ActorID, "subject_ref": r.SubjectRef, "summary": r.Summary,
		"correlation_id": r.CorrelationID, "payload": r.Payload,
	}
}

// RegisterEntries lists GDPR deletions for a system admin.
func (s *Service) RegisterEntries(ctx context.Context, f repository.RegisterFilter, after *repository.RegisterCursor, limit int) (Page[domain.Register, repository.RegisterCursor], error) {
	var page Page[domain.Register, repository.RegisterCursor]
	if err := requireSystemAdmin(ctx); err != nil {
		return page, err
	}
	if f.Status != "" && !f.Status.Valid() {
		return page, ycerrors.Validation().Field("status", "invalid", "Status is in_progress, complete or incident").Build()
	}
	err := dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		items, err := s.repo.ListRegister(ctx, tx, f, after, limit+1)
		if err != nil {
			return err
		}
		if len(items) > limit {
			items = items[:limit]
			last := items[limit-1]
			page.Next = &repository.RegisterCursor{InitiatedAt: last.InitiatedAt, UserID: last.UserID}
		}
		page.Items = items
		return nil
	})
	return page, err
}

// RegisterEntry returns one user's deletion with each service's report.
func (s *Service) RegisterEntry(ctx context.Context, userID string) (domain.Register, error) {
	if err := requireSystemAdmin(ctx); err != nil {
		return domain.Register{}, err
	}
	var reg domain.Register
	err := dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		var ok bool
		var err error
		reg, ok, err = s.repo.Register(ctx, tx, "", userID, false)
		if err == nil && !ok {
			err = ycerrors.NotFound("GDPR deletion", userID)
		}
		return err
	})
	return reg, err
}

func validCategories(cs []domain.Category) error {
	if i := slices.IndexFunc(cs, func(c domain.Category) bool { return !c.Valid() }); i >= 0 {
		return ycerrors.Validation().Field("category", "invalid", "Unknown category "+string(cs[i])).Build()
	}
	return nil
}
