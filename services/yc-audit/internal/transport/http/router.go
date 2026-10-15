// Package http is yc-audit's HTTP transport. Routing, parameter parsing
// and request/response types are generated from api/openapi.yaml.
package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	api "github.com/slaghuis/yc-platform/services/yc-audit/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-audit/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-audit/internal/repository"
	"github.com/slaghuis/yc-platform/services/yc-audit/internal/service"
)

// Audit is the service the handlers call.
type Audit interface {
	Records(ctx context.Context, f repository.RecordFilter, after *repository.Cursor, limit int) (service.Page[domain.Record, repository.Cursor], error)
	ShopRecords(ctx context.Context, f repository.RecordFilter, after *repository.Cursor, limit int) (service.Page[domain.Record, repository.Cursor], error)
	RecordByID(ctx context.Context, id string) (domain.Record, error)
	Export(ctx context.Context, f repository.RecordFilter, format string) ([]byte, bool, error)
	RegisterEntries(ctx context.Context, f repository.RegisterFilter, after *repository.RegisterCursor, limit int) (service.Page[domain.Register, repository.RegisterCursor], error)
	RegisterEntry(ctx context.Context, userID string) (domain.Register, error)
}

// NewRouter returns the service's routes on the standard httpkit chain.
func NewRouter(svc Audit, build httpkit.BuildInfo, checks []httpkit.Check) http.Handler {
	r := httpkit.NewRouter(httpkit.RouterOptions{
		Build:  build,
		Checks: checks,
		Middleware: []func(http.Handler) http.Handler{
			authctx.Middleware, authctx.Audience, httpkit.LimitBody(httpkit.MaxBodyBytes),
		},
	})
	strict := api.NewStrictHandlerWithOptions(handlers{svc: svc}, nil, api.StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  httpkit.RequestError,
		ResponseErrorHandlerFunc: httpkit.ResponseError,
	})
	api.HandlerWithOptions(strict, api.ChiServerOptions{BaseRouter: r, ErrorHandlerFunc: httpkit.ParamError})
	return r
}

// Routes are the gateway routes yc-audit registers (ADR-020 rule 7).
func Routes() []httpkit.RegisteredRoute {
	sysadmin := []string{string(authctx.ActorSystemAdmin)}
	get := []string{http.MethodGet}
	return []httpkit.RegisteredRoute{
		{Prefix: "/sysadmin/audit", Roles: sysadmin, Methods: get},
		{Prefix: "/sysadmin/gdpr", Roles: sysadmin, Methods: get},
		{Prefix: "/admin/audit", Roles: []string{string(authctx.ActorShopAdmin)}, Methods: get},
	}
}

type handlers struct {
	svc Audit
}

var _ api.StrictServerInterface = handlers{}

func (h handlers) ListAuditRecords(ctx context.Context, req api.ListAuditRecordsRequestObject) (api.ListAuditRecordsResponseObject, error) {
	p := req.Params
	size, after, err := page[repository.Cursor](p.PageSize, p.Cursor)
	if err != nil {
		return nil, err
	}
	f := filter(p.Category, p.EventType, p.SubjectRef, p.From, p.To)
	f.TenantID = deref(p.TenantID)
	res, err := h.svc.Records(ctx, f, after, size)
	if err != nil {
		return nil, err
	}
	return api.ListAuditRecords200JSONResponse(recordList(res, size, p.Cursor)), nil
}

func (h handlers) ListShopAuditRecords(ctx context.Context, req api.ListShopAuditRecordsRequestObject) (api.ListShopAuditRecordsResponseObject, error) {
	p := req.Params
	size, after, err := page[repository.Cursor](p.PageSize, p.Cursor)
	if err != nil {
		return nil, err
	}
	res, err := h.svc.ShopRecords(ctx, filter(p.Category, p.EventType, p.SubjectRef, p.From, p.To), after, size)
	if err != nil {
		return nil, err
	}
	return api.ListShopAuditRecords200JSONResponse(recordList(res, size, p.Cursor)), nil
}

func (h handlers) ExportAuditRecords(ctx context.Context, req api.ExportAuditRecordsRequestObject) (api.ExportAuditRecordsResponseObject, error) {
	p := req.Params
	f := filter(p.Category, p.EventType, p.SubjectRef, p.From, p.To)
	f.TenantID = deref(p.TenantID)
	format := service.FormatCSV
	if p.Format != nil {
		format = string(*p.Format)
	}
	body, truncated, err := h.svc.Export(ctx, f, format)
	if err != nil {
		return nil, err
	}
	headers := api.ExportAuditRecords200ResponseHeaders{XYCTruncated: &truncated}
	if format == service.FormatJSONL {
		return api.ExportAuditRecords200ApplicationXNdjsonResponse{
			Body: bytes.NewReader(body), Headers: headers, ContentLength: int64(len(body)),
		}, nil
	}
	return api.ExportAuditRecords200TextCsvResponse{
		Body: bytes.NewReader(body), Headers: headers, ContentLength: int64(len(body)),
	}, nil
}

func (h handlers) GetAuditRecord(ctx context.Context, req api.GetAuditRecordRequestObject) (api.GetAuditRecordResponseObject, error) {
	rec, err := h.svc.RecordByID(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	out := record(rec)
	var payload map[string]any
	if err := json.Unmarshal(rec.Payload, &payload); err == nil {
		out.Payload = &payload
	}
	return api.GetAuditRecord200JSONResponse(out), nil
}

func (h handlers) ListGdprRegister(ctx context.Context, req api.ListGdprRegisterRequestObject) (api.ListGdprRegisterResponseObject, error) {
	p := req.Params
	size, after, err := page[repository.RegisterCursor](p.PageSize, p.Cursor)
	if err != nil {
		return nil, err
	}
	var f repository.RegisterFilter
	if p.Status != nil {
		f.Status = domain.RegisterStatus(*p.Status)
	}
	res, err := h.svc.RegisterEntries(ctx, f, after, size)
	if err != nil {
		return nil, err
	}
	return api.ListGdprRegister200JSONResponse(registerList(res, size, p.Cursor)), nil
}

func (h handlers) ListGdprIncidents(ctx context.Context, req api.ListGdprIncidentsRequestObject) (api.ListGdprIncidentsResponseObject, error) {
	p := req.Params
	size, after, err := page[repository.RegisterCursor](p.PageSize, p.Cursor)
	if err != nil {
		return nil, err
	}
	res, err := h.svc.RegisterEntries(ctx, repository.RegisterFilter{Incidents: true}, after, size)
	if err != nil {
		return nil, err
	}
	return api.ListGdprIncidents200JSONResponse(registerList(res, size, p.Cursor)), nil
}

func (h handlers) GetGdprRegisterEntry(ctx context.Context, req api.GetGdprRegisterEntryRequestObject) (api.GetGdprRegisterEntryResponseObject, error) {
	reg, err := h.svc.RegisterEntry(ctx, req.UserID.String())
	if err != nil {
		return nil, err
	}
	out := registerEntry(reg)
	reports := make([]api.ServiceReport, len(reg.Reports))
	for i, r := range reg.Reports {
		reports[i] = api.ServiceReport{
			Service: r.Service, Action: api.ServiceReportAction(r.Action), RecordCount: r.RecordCount,
			CompletedAt: r.CompletedAt.UTC(), ReportedAt: r.ReportedAt.UTC(),
		}
	}
	out.Reports = &reports
	return api.GetGdprRegisterEntry200JSONResponse(out), nil
}

func filter(categories *[]api.Category, eventType, subject *string, from, to *time.Time) repository.RecordFilter {
	f := repository.RecordFilter{EventType: deref(eventType), SubjectRef: deref(subject), From: from, To: to}
	if categories != nil {
		for _, c := range *categories {
			f.Categories = append(f.Categories, domain.Category(c))
		}
	}
	return f
}

// page parses the page size and cursor.
func page[C any](size *int, cursor *string) (int, *C, error) {
	p, err := httpkit.ParsePage(size, cursor)
	if err != nil {
		return 0, nil, err
	}
	if p.Cursor == "" {
		return p.Size, nil, nil
	}
	after := new(C)
	if err := httpkit.DecodeCursor(p.Cursor, after); err != nil {
		return 0, nil, err
	}
	return p.Size, after, nil
}

func pageInfo[C any](size int, cursor *string, next *C) httpkit.PageInfo {
	info := httpkit.PageInfo{Size: size, Cursor: deref(cursor)}
	if next != nil {
		info.NextCursor = httpkit.EncodeCursor(next)
	}
	return info
}

func recordList(p service.Page[domain.Record, repository.Cursor], size int, cursor *string) api.AuditRecordList {
	out := api.AuditRecordList{Items: make([]api.AuditRecord, len(p.Items)), Page: pageInfo(size, cursor, p.Next)}
	for i, r := range p.Items {
		out.Items[i] = record(r)
	}
	return out
}

func record(r domain.Record) api.AuditRecord {
	return api.AuditRecord{
		ID: mustUUID(r.ID), TenantID: r.TenantID, Category: api.Category(r.Category), EventType: r.EventType,
		EventID: mustUUID(r.EventID), SourceService: r.SourceService, ActorType: optional(r.ActorType),
		ActorID: optional(r.ActorID), SubjectRef: optional(r.SubjectRef), Summary: r.Summary,
		CorrelationID: optional(r.CorrelationID), EventTime: r.EventTime.UTC(), RecordedAt: r.RecordedAt.UTC(),
	}
}

func registerList(p service.Page[domain.Register, repository.RegisterCursor], size int, cursor *string) api.RegisterList {
	out := api.RegisterList{Items: make([]api.RegisterEntry, len(p.Items)), Page: pageInfo(size, cursor, p.Next)}
	for i, r := range p.Items {
		out.Items[i] = registerEntry(r)
	}
	return out
}

func registerEntry(r domain.Register) api.RegisterEntry {
	missing := r.Missing()
	if missing == nil {
		missing = []string{}
	}
	return api.RegisterEntry{
		TenantID: r.TenantID, UserID: mustUUID(r.UserID), Pseudonym: mustUUID(r.Pseudonym), Role: r.Role,
		RequestedBy: r.RequestedBy, Status: api.RegisterStatus(r.Status), InitiatedAt: r.InitiatedAt.UTC(),
		Deadline: r.Deadline.UTC(), CompletedAt: utc(r.CompletedAt), IncidentAt: utc(r.IncidentAt),
		ExpectedServices: r.Expected, MissingServices: missing,
	}
}

func mustUUID(s string) uuid.UUID {
	id, _ := uuid.Parse(s)
	return id
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
