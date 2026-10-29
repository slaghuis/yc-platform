// Package http is yc-analytics' HTTP transport. Routing, parameter parsing
// and request/response types are generated from api/openapi.yaml.
package http

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	api "github.com/slaghuis/yc-platform/services/yc-analytics/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-analytics/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-analytics/internal/service"
)

// Analytics is the service the handlers call.
type Analytics interface {
	Range(from, to *time.Time) (domain.Range, error)
	Dashboard(ctx context.Context, r domain.Range) (service.Dashboard, error)
	Revenue(ctx context.Context, r domain.Range) ([]domain.RevenueRow, error)
	TopProducts(ctx context.Context, r domain.Range, by string, limit int) ([]domain.ProductRow, error)
	Funnel(ctx context.Context, r domain.Range) (domain.FunnelRow, []domain.FunnelRow, error)
	CartRecovery(ctx context.Context, r domain.Range) (domain.CartRecovery, error)
	Gateways(ctx context.Context, r domain.Range) ([]domain.GatewayRow, error)
	Export(ctx context.Context, query string, r domain.Range) (string, []byte, error)
	CompareTenants(ctx context.Context, r domain.Range) ([]service.TenantComparison, error)
	Rebuild(ctx context.Context, tenantID string, r domain.Range) (service.RebuildResult, error)
	CheckConsistency(ctx context.Context, tenantID string, r domain.Range) (service.Consistency, error)
}

// NewRouter returns the service's routes on the standard httpkit chain.
// Nothing here creates resources, so there is no idempotency store:
// rebuilding is idempotent.
func NewRouter(svc Analytics, build httpkit.BuildInfo, checks []httpkit.Check) http.Handler {
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

// Routes are the gateway routes yc-analytics registers (ADR-020 rule 7).
func Routes() []httpkit.RegisteredRoute {
	return []httpkit.RegisteredRoute{
		{Prefix: "/admin/analytics", Methods: []string{http.MethodGet}, Roles: []string{string(authctx.ActorShopAdmin)}},
		{Prefix: "/sysadmin/analytics", Methods: []string{http.MethodGet, http.MethodPost}, Roles: []string{string(authctx.ActorSystemAdmin)}},
	}
}

type handlers struct {
	svc Analytics
}

var _ api.StrictServerInterface = handlers{}

func (h handlers) GetShopDashboard(ctx context.Context, req api.GetShopDashboardRequestObject) (api.GetShopDashboardResponseObject, error) {
	out, err := h.dashboard(ctx, req.Params.From, req.Params.To)
	if err != nil {
		return nil, err
	}
	return api.GetShopDashboard200JSONResponse(out), nil
}

func (h handlers) GetPlatformDashboard(ctx context.Context, req api.GetPlatformDashboardRequestObject) (api.GetPlatformDashboardResponseObject, error) {
	out, err := h.dashboard(ctx, req.Params.From, req.Params.To)
	if err != nil {
		return nil, err
	}
	return api.GetPlatformDashboard200JSONResponse(out), nil
}

func (h handlers) dashboard(ctx context.Context, from, to *openapi_types.Date) (api.Dashboard, error) {
	r, err := h.svc.Range(dates(from, to))
	if err != nil {
		return api.Dashboard{}, err
	}
	d, err := h.svc.Dashboard(ctx, r)
	if err != nil {
		return api.Dashboard{}, err
	}
	return api.Dashboard{
		Range: dateRange(d.Range), Revenue: mapAll(d.Revenue, revenue), Funnel: funnel(d.Funnel),
		TopProducts: mapAll(d.TopProducts, product), Gateways: mapAll(d.Gateways, gateway),
		PendingDays: d.PendingDays,
	}, nil
}

func (h handlers) GetRevenue(ctx context.Context, req api.GetRevenueRequestObject) (api.GetRevenueResponseObject, error) {
	r, err := h.svc.Range(dates(req.Params.From, req.Params.To))
	if err != nil {
		return nil, err
	}
	rows, err := h.svc.Revenue(ctx, r)
	if err != nil {
		return nil, err
	}
	return api.GetRevenue200JSONResponse{Range: dateRange(r), Days: mapAll(rows, revenue)}, nil
}

func (h handlers) GetTopProducts(ctx context.Context, req api.GetTopProductsRequestObject) (api.GetTopProductsResponseObject, error) {
	p := req.Params
	r, err := h.svc.Range(dates(p.From, p.To))
	if err != nil {
		return nil, err
	}
	rows, err := h.svc.TopProducts(ctx, r, string(deref(p.By)), deref(p.Limit))
	if err != nil {
		return nil, err
	}
	return api.GetTopProducts200JSONResponse{Range: dateRange(r), Products: mapAll(rows, product)}, nil
}

func (h handlers) GetFunnel(ctx context.Context, req api.GetFunnelRequestObject) (api.GetFunnelResponseObject, error) {
	r, err := h.svc.Range(dates(req.Params.From, req.Params.To))
	if err != nil {
		return nil, err
	}
	total, daily, err := h.svc.Funnel(ctx, r)
	if err != nil {
		return nil, err
	}
	return api.GetFunnel200JSONResponse{Range: dateRange(r), Total: funnel(total), Days: mapAll(daily, funnel)}, nil
}

func (h handlers) GetCartRecovery(ctx context.Context, req api.GetCartRecoveryRequestObject) (api.GetCartRecoveryResponseObject, error) {
	r, err := h.svc.Range(dates(req.Params.From, req.Params.To))
	if err != nil {
		return nil, err
	}
	v, err := h.svc.CartRecovery(ctx, r)
	if err != nil {
		return nil, err
	}
	return api.GetCartRecovery200JSONResponse{
		Range: dateRange(r), CartsCreated: v.CartsCreated, Abandoned: v.Abandoned, Messaged: v.Messaged,
		Recovered: v.Recovered, AbandonmentRate: v.AbandonmentRate, RecoveryRate: v.RecoveryRate,
		RecoveryWindow: v.RecoveryWindow.String(),
	}, nil
}

func (h handlers) GetGateways(ctx context.Context, req api.GetGatewaysRequestObject) (api.GetGatewaysResponseObject, error) {
	r, err := h.svc.Range(dates(req.Params.From, req.Params.To))
	if err != nil {
		return nil, err
	}
	rows, err := h.svc.Gateways(ctx, r)
	if err != nil {
		return nil, err
	}
	return api.GetGateways200JSONResponse{Range: dateRange(r), Gateways: mapAll(rows, gateway)}, nil
}

func (h handlers) ExportReport(ctx context.Context, req api.ExportReportRequestObject) (api.ExportReportResponseObject, error) {
	r, err := h.svc.Range(dates(req.Params.From, req.Params.To))
	if err != nil {
		return nil, err
	}
	name, body, err := h.svc.Export(ctx, string(req.Query), r)
	if err != nil {
		return nil, err
	}
	disposition := fmt.Sprintf("attachment; filename=%q", name)
	return api.ExportReport200TextCsvResponse{
		Body: bytes.NewReader(body), ContentLength: int64(len(body)),
		Headers: api.ExportReport200ResponseHeaders{ContentDisposition: &disposition},
	}, nil
}

func (h handlers) CompareTenants(ctx context.Context, req api.CompareTenantsRequestObject) (api.CompareTenantsResponseObject, error) {
	r, err := h.svc.Range(dates(req.Params.From, req.Params.To))
	if err != nil {
		return nil, err
	}
	rows, err := h.svc.CompareTenants(ctx, r)
	if err != nil {
		return nil, err
	}
	out := api.TenantComparisonList{Range: dateRange(r), Tenants: make([]api.TenantComparison, len(rows))}
	for i, c := range rows {
		out.Tenants[i] = api.TenantComparison{
			TenantID: c.TenantID, Name: c.Name, Revenue: mapAll(c.Revenue, revenue), Funnel: funnel(c.Funnel),
		}
	}
	return api.CompareTenants200JSONResponse(out), nil
}

func (h handlers) RebuildAggregates(ctx context.Context, req api.RebuildAggregatesRequestObject) (api.RebuildAggregatesResponseObject, error) {
	b := req.Body
	r, err := h.svc.Range(&b.From.Time, &b.To.Time)
	if err != nil {
		return nil, err
	}
	res, err := h.svc.Rebuild(ctx, deref(b.TenantID), r)
	if err != nil {
		return nil, err
	}
	return api.RebuildAggregates200JSONResponse{
		TenantID: optional(res.TenantID), Range: dateRange(res.Range), Aggregates: domain.Aggregates, Rows: res.Rows,
	}, nil
}

func (h handlers) CheckConsistency(ctx context.Context, req api.CheckConsistencyRequestObject) (api.CheckConsistencyResponseObject, error) {
	p := req.Params
	r, err := h.svc.Range(dates(p.From, p.To))
	if err != nil {
		return nil, err
	}
	c, err := h.svc.CheckConsistency(ctx, deref(p.TenantID), r)
	if err != nil {
		return nil, err
	}
	out := api.Consistency{
		Range: dateRange(c.Range), Consistent: len(c.Mismatches) == 0, PendingDays: c.PendingDays,
		Mismatches: make([]api.Mismatch, len(c.Mismatches)),
	}
	for i, m := range c.Mismatches {
		out.Mismatches[i] = api.Mismatch{Aggregate: m.Aggregate, TenantID: m.TenantID, Date: openapi_types.Date{Time: m.Date}}
	}
	return api.CheckConsistency200JSONResponse(out), nil
}

func revenue(v domain.RevenueRow) api.Revenue {
	return api.Revenue{
		Date: date(v.Date), Currency: v.Currency, Orders: v.Orders, Cancelled: v.Cancelled, ItemsMinor: v.ItemsMinor,
		TaxMinor: v.TaxMinor, ShippingMinor: v.ShippingMinor, GrossMinor: v.GrossMinor,
		RefundedMinor: v.RefundedMinor, NetMinor: v.NetMinor, AverageOrderMinor: v.AverageOrderMinor(),
	}
}

func funnel(v domain.FunnelRow) api.Funnel {
	return api.Funnel{
		Date: date(v.Date), Visitors: v.Visitors, CartsCreated: v.CartsCreated, CheckoutsStarted: v.CheckoutsStarted,
		CheckoutsConfirmed: v.CheckoutsConfirmed, OrdersPlaced: v.OrdersPlaced, ConversionRate: v.Conversion(),
	}
}

func product(v domain.ProductRow) api.Product {
	return api.Product{
		SkuID: uuid.MustParse(v.SKUID), Name: v.Name, Currency: v.Currency, Units: v.Units, RevenueMinor: v.RevenueMinor,
	}
}

func gateway(v domain.GatewayRow) api.Gateway {
	return api.Gateway{
		Gateway: v.Gateway, Currency: v.Currency, Attempts: v.Attempts, Successes: v.Successes,
		Failures: v.Failures, AmountMinor: v.AmountMinor, SuccessRate: v.SuccessRate(),
	}
}

func dateRange(r domain.Range) api.DateRange {
	return api.DateRange{From: openapi_types.Date{Time: r.From}, To: openapi_types.Date{Time: r.To}}
}

func dates(from, to *openapi_types.Date) (*time.Time, *time.Time) {
	var f, t *time.Time
	if from != nil {
		f = &from.Time
	}
	if to != nil {
		t = &to.Time
	}
	return f, t
}

func date(t time.Time) *openapi_types.Date {
	if t.IsZero() {
		return nil
	}
	return &openapi_types.Date{Time: t}
}

func mapAll[T, U any](in []T, fn func(T) U) []U {
	out := make([]U, len(in))
	for i, v := range in {
		out[i] = fn(v)
	}
	return out
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
