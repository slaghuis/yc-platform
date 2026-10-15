// Package http is yc-monitor's HTTP transport. Routing, parameter parsing
// and request/response types are generated from api/openapi.yaml.
package http

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	api "github.com/slaghuis/yc-platform/services/yc-monitor/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-monitor/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-monitor/internal/repository"
	"github.com/slaghuis/yc-platform/services/yc-monitor/internal/service"
)

// Monitor is the service the handlers call.
type Monitor interface {
	Dashboard(ctx context.Context) (service.Dashboard, error)
	Services(ctx context.Context) ([]domain.Service, error)
	Series(ctx context.Context, q domain.MetricQuery) ([]domain.Point, error)
	Alerts(ctx context.Context, f domain.AlertFilter, after *repository.AlertCursor, limit int) (service.Page[domain.Alert, repository.AlertCursor], error)
	Acknowledge(ctx context.Context, id string) (domain.Alert, error)
	Thresholds(ctx context.Context) ([]domain.Thresholds, error)
	PutThresholds(ctx context.Context, in service.ThresholdsInput) (domain.Thresholds, error)
	ListDeadLetters(ctx context.Context, f domain.Filter) ([]domain.DLQEntry, error)
	DeadLetterSummary(ctx context.Context) ([]domain.DLQSummary, error)
	AcknowledgeDeadLetter(ctx context.Context, sequence uint64) (domain.DLQEntry, error)
	ListEvents(ctx context.Context, f domain.Filter) ([]domain.ReceivedEvent, error)
}

// NewRouter returns the service's routes on the standard httpkit chain.
// Nothing here creates resources, so there is no idempotency store:
// acknowledging and setting thresholds are idempotent.
func NewRouter(svc Monitor, build httpkit.BuildInfo, checks []httpkit.Check) http.Handler {
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

// Routes are the gateway routes yc-monitor registers (ADR-020 rule 7).
func Routes() []httpkit.RegisteredRoute {
	sysadmin := []string{string(authctx.ActorSystemAdmin)}
	shopAdmin := []string{string(authctx.ActorShopAdmin)}
	routes := make([]httpkit.RegisteredRoute, 0, 10)
	for _, p := range []string{"dashboard", "services", "metrics", "alerts", "thresholds", "dlq", "events"} {
		routes = append(routes, httpkit.RegisteredRoute{Prefix: "/sysadmin/" + p, Roles: sysadmin})
	}
	for _, p := range []string{"metrics", "alerts", "dlq"} {
		routes = append(routes, httpkit.RegisteredRoute{Prefix: "/admin/" + p, Roles: shopAdmin})
	}
	return routes
}

type handlers struct {
	svc Monitor
}

var _ api.StrictServerInterface = handlers{}

func (h handlers) GetDashboard(ctx context.Context, _ api.GetDashboardRequestObject) (api.GetDashboardResponseObject, error) {
	d, err := h.svc.Dashboard(ctx)
	if err != nil {
		return nil, err
	}
	out := api.Dashboard{
		Status: api.DashboardStatus(d.Status), DeadLettersUnacknowledged: d.DeadLettersUnacknowledged,
		LastHour: d.LastHour, GeneratedAt: d.GeneratedAt,
	}
	out.Services.Healthy = d.Services[domain.HealthHealthy]
	out.Services.Unhealthy = d.Services[domain.HealthUnhealthy]
	out.Services.Unknown = d.Services[domain.HealthUnknown]
	out.Services.Deregistered = d.Services[domain.HealthDeregistered]
	for _, n := range d.Services {
		out.Services.Total += n
	}
	out.OpenAlerts.Critical = d.OpenAlerts[domain.SeverityCritical]
	out.OpenAlerts.Warning = d.OpenAlerts[domain.SeverityWarning]
	out.OpenAlerts.Info = d.OpenAlerts[domain.SeverityInfo]
	return api.GetDashboard200JSONResponse(out), nil
}

func (h handlers) ListServices(ctx context.Context, _ api.ListServicesRequestObject) (api.ListServicesResponseObject, error) {
	all, err := h.svc.Services(ctx)
	if err != nil {
		return nil, err
	}
	out := api.ServiceList{Items: make([]api.Service, len(all))}
	for i, s := range all {
		out.Items[i] = api.Service{
			Name: s.Name, InstanceID: s.InstanceID, BaseURL: s.BaseURL, Version: optional(s.Version),
			Status: api.HealthStatus(s.Status), ConsecutiveFailures: s.ConsecutiveFailures,
			ConsecutiveSuccesses: s.ConsecutiveSuccesses, LastError: optional(s.LastError),
			LastCheckAt: s.LastCheckAt, LastHealthyAt: s.LastHealthyAt, UnhealthySince: s.UnhealthySince,
			RegisteredAt: s.RegisteredAt, DeregisteredAt: s.DeregisteredAt,
		}
	}
	return api.ListServices200JSONResponse(out), nil
}

func (h handlers) GetMetrics(ctx context.Context, req api.GetMetricsRequestObject) (api.GetMetricsResponseObject, error) {
	p := req.Params
	out, err := h.series(ctx, string(p.Metric), deref(p.TenantID), (*string)(p.Bucket), p.From, p.To)
	if err != nil {
		return nil, err
	}
	return api.GetMetrics200JSONResponse(out), nil
}

func (h handlers) GetShopMetrics(ctx context.Context, req api.GetShopMetricsRequestObject) (api.GetShopMetricsResponseObject, error) {
	p := req.Params
	out, err := h.series(ctx, string(p.Metric), "", (*string)(p.Bucket), p.From, p.To)
	if err != nil {
		return nil, err
	}
	return api.GetShopMetrics200JSONResponse(out), nil
}

func (h handlers) series(ctx context.Context, metric, tenantID string, bucket *string, from, to *time.Time) (api.MetricSeries, error) {
	q := domain.MetricQuery{Metric: metric, TenantID: tenantID, Bucket: domain.Bucket(deref(bucket))}
	if from != nil {
		q.From = from.UTC()
	}
	if to != nil {
		q.To = to.UTC()
	}
	points, err := h.svc.Series(ctx, q)
	if err != nil {
		return api.MetricSeries{}, err
	}
	if q.Bucket == "" {
		q.Bucket = domain.Bucket1h
	}
	out := api.MetricSeries{
		Metric: api.Metric(metric), TenantID: optional(tenantID), Bucket: string(q.Bucket),
		Points: make([]api.MetricPoint, len(points)),
	}
	for i, p := range points {
		out.Points[i] = api.MetricPoint{BucketStart: p.BucketStart, Value: p.Value}
	}
	if to != nil {
		out.To = *to
	} else {
		out.To = time.Now().UTC()
	}
	if from != nil {
		out.From = *from
	} else {
		out.From = q.Bucket.Start(out.To).Add(-59 * q.Bucket.Duration())
	}
	return out, nil
}

func (h handlers) ListAlerts(ctx context.Context, req api.ListAlertsRequestObject) (api.ListAlertsResponseObject, error) {
	p := req.Params
	f := alertFilter((*string)(p.Status), (*string)(p.Severity), p.Rule)
	f.TenantID = deref(p.TenantID)
	out, err := h.alerts(ctx, f, p.PageSize, p.Cursor)
	if err != nil {
		return nil, err
	}
	return api.ListAlerts200JSONResponse(out), nil
}

func (h handlers) ListShopAlerts(ctx context.Context, req api.ListShopAlertsRequestObject) (api.ListShopAlertsResponseObject, error) {
	p := req.Params
	out, err := h.alerts(ctx, alertFilter((*string)(p.Status), (*string)(p.Severity), p.Rule), p.PageSize, p.Cursor)
	if err != nil {
		return nil, err
	}
	return api.ListShopAlerts200JSONResponse(out), nil
}

func (h handlers) alerts(ctx context.Context, f domain.AlertFilter, pageSize *int, cursor *string) (api.AlertList, error) {
	size, after, err := page[repository.AlertCursor](pageSize, cursor)
	if err != nil {
		return api.AlertList{}, err
	}
	res, err := h.svc.Alerts(ctx, f, after, size)
	if err != nil {
		return api.AlertList{}, err
	}
	out := api.AlertList{Items: make([]api.Alert, len(res.Items)), Page: pageInfo(size, cursor, res.Next)}
	for i, a := range res.Items {
		out.Items[i] = alert(a)
	}
	return out, nil
}

func (h handlers) AcknowledgeAlert(ctx context.Context, req api.AcknowledgeAlertRequestObject) (api.AcknowledgeAlertResponseObject, error) {
	a, err := h.svc.Acknowledge(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.AcknowledgeAlert200JSONResponse(alert(a)), nil
}

func (h handlers) AcknowledgeShopAlert(ctx context.Context, req api.AcknowledgeShopAlertRequestObject) (api.AcknowledgeShopAlertResponseObject, error) {
	a, err := h.svc.Acknowledge(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.AcknowledgeShopAlert200JSONResponse(alert(a)), nil
}

func (h handlers) ListThresholds(ctx context.Context, _ api.ListThresholdsRequestObject) (api.ListThresholdsResponseObject, error) {
	all, err := h.svc.Thresholds(ctx)
	if err != nil {
		return nil, err
	}
	out := api.ThresholdList{Items: make([]api.Thresholds, len(all))}
	for i, t := range all {
		out.Items[i] = thresholds(t)
	}
	return api.ListThresholds200JSONResponse(out), nil
}

func (h handlers) PutThresholds(ctx context.Context, req api.PutThresholdsRequestObject) (api.PutThresholdsResponseObject, error) {
	b := req.Body
	t, err := h.svc.PutThresholds(ctx, service.ThresholdsInput{
		Rule: string(b.Rule), TenantID: deref(b.TenantID), Threshold: b.Threshold, MinVolume: b.MinVolume,
		WindowMinutes: b.WindowMinutes, Enabled: b.Enabled,
	})
	if err != nil {
		return nil, err
	}
	return api.PutThresholds200JSONResponse(thresholds(t)), nil
}

func (h handlers) ListDeadLetters(ctx context.Context, req api.ListDeadLettersRequestObject) (api.ListDeadLettersResponseObject, error) {
	p := req.Params
	entries, err := h.svc.ListDeadLetters(ctx, domain.Filter{
		TenantID: deref(p.TenantID), Service: deref(p.Service), Acknowledged: p.Acknowledged, Limit: deref(p.Limit),
	})
	if err != nil {
		return nil, err
	}
	out := api.DeadLetterList{Entries: make([]api.DeadLetter, len(entries))}
	for i, d := range entries {
		out.Entries[i] = deadLetter(d)
	}
	return api.ListDeadLetters200JSONResponse(out), nil
}

func (h handlers) GetDeadLetterSummary(ctx context.Context, _ api.GetDeadLetterSummaryRequestObject) (api.GetDeadLetterSummaryResponseObject, error) {
	out, err := h.summary(ctx)
	if err != nil {
		return nil, err
	}
	return api.GetDeadLetterSummary200JSONResponse(out), nil
}

func (h handlers) GetShopDeadLetterSummary(ctx context.Context, _ api.GetShopDeadLetterSummaryRequestObject) (api.GetShopDeadLetterSummaryResponseObject, error) {
	out, err := h.summary(ctx)
	if err != nil {
		return nil, err
	}
	return api.GetShopDeadLetterSummary200JSONResponse(out), nil
}

func (h handlers) summary(ctx context.Context) (api.DeadLetterSummary, error) {
	rows, err := h.svc.DeadLetterSummary(ctx)
	if err != nil {
		return api.DeadLetterSummary{}, err
	}
	out := api.DeadLetterSummary{Items: make([]api.DeadLetterCount, len(rows))}
	for i, r := range rows {
		out.Items[i] = api.DeadLetterCount{
			TenantID: r.TenantID, Service: r.Service, Unacknowledged: r.Unacknowledged, Latest: r.Latest.UTC(),
		}
	}
	return out, nil
}

func (h handlers) AcknowledgeDeadLetter(ctx context.Context, req api.AcknowledgeDeadLetterRequestObject) (api.AcknowledgeDeadLetterResponseObject, error) {
	d, err := h.svc.AcknowledgeDeadLetter(ctx, uint64(req.Sequence)) //nolint:gosec // the spec requires a positive sequence
	if err != nil {
		return nil, err
	}
	return api.AcknowledgeDeadLetter200JSONResponse(deadLetter(d)), nil
}

func (h handlers) ListReceivedEvents(ctx context.Context, req api.ListReceivedEventsRequestObject) (api.ListReceivedEventsResponseObject, error) {
	p := req.Params
	all, err := h.svc.ListEvents(ctx, domain.Filter{TenantID: deref(p.TenantID), Limit: deref(p.Limit)})
	if err != nil {
		return nil, err
	}
	out := api.ReceivedEventList{Events: make([]api.ReceivedEvent, len(all))}
	for i, e := range all {
		out.Events[i] = api.ReceivedEvent{
			EventID: e.EventID, TenantID: e.TenantID, EventType: e.EventType, SourceService: e.SourceService,
			CorrelationID: optional(e.CorrelationID), EventTime: e.EventTime.UTC(), Envelope: object(e.Envelope),
			ReceivedAt: e.ReceivedAt.UTC(),
		}
	}
	return api.ListReceivedEvents200JSONResponse(out), nil
}

func alertFilter(status, severity, rule *string) domain.AlertFilter {
	return domain.AlertFilter{
		Status: domain.AlertStatus(deref(status)), Severity: domain.Severity(deref(severity)), Rule: deref(rule),
	}
}

func alert(a domain.Alert) api.Alert {
	out := api.Alert{
		ID: uuid.MustParse(a.ID), TenantID: a.TenantID, Rule: a.Rule, Metric: a.Metric,
		Severity: api.Severity(a.Severity), Status: api.AlertStatus(a.Status()), Summary: a.Summary,
		Value: a.Value, Threshold: a.Threshold, Notify: a.Notify, Occurrences: a.Occurrences,
		OpenedAt: a.OpenedAt.UTC(), LastSeenAt: a.LastSeenAt.UTC(), AcknowledgedAt: a.AcknowledgedAt,
		AcknowledgedBy: optional(a.AcknowledgedBy), ResolvedAt: a.ResolvedAt,
	}
	if out.Notify == nil {
		out.Notify = []string{}
	}
	if len(a.Dimensions) > 0 {
		out.Dimensions = &a.Dimensions
	}
	return out
}

func thresholds(t domain.Thresholds) api.Thresholds {
	return api.Thresholds{
		TenantID: t.TenantID, Rule: t.Rule, Threshold: t.Threshold, MinVolume: t.MinVolume,
		WindowMinutes: t.WindowMinutes, Enabled: t.Enabled, UpdatedBy: optional(t.UpdatedBy), UpdatedAt: t.UpdatedAt,
	}
}

func deadLetter(d domain.DLQEntry) api.DeadLetter {
	return api.DeadLetter{
		ID: uuid.MustParse(d.ID), DlqSequence: int64(d.DLQSequence), //nolint:gosec // stream sequences fit in int64
		TenantID: d.TenantID, Service: d.Service, Consumer: d.Consumer, EventType: d.EventType, EventID: d.EventID,
		FailureReason: d.FailureReason, Attempts: d.Attempts, LastError: optional(d.LastError),
		FirstFailureAt: nonZero(d.FirstFailureAt), LastFailureAt: nonZero(d.LastFailureAt),
		Envelope: object(d.Envelope), CreatedAt: d.CreatedAt.UTC(), Acknowledged: d.Acknowledged,
		AcknowledgedBy: optional(d.AcknowledgedBy), AcknowledgedAt: d.AcknowledgedAt,
	}
}

func object(raw json.RawMessage) map[string]any {
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out
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

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nonZero(t time.Time) *time.Time {
	if t.IsZero() {
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
