// Package http is yc-tenant's HTTP transport. Routing, parameter parsing
// and request/response types are generated from api/openapi.yaml.
package http

import (
	"context"
	"net/http"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	api "github.com/slaghuis/yc-platform/services/yc-tenant/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-tenant/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-tenant/internal/repository"
	"github.com/slaghuis/yc-platform/services/yc-tenant/internal/service"
)

// Tenants is the service the handlers call.
type Tenants interface {
	Create(ctx context.Context, in domain.CreateInput) (domain.Tenant, error)
	Get(ctx context.Context, tenantID string) (domain.Tenant, error)
	List(ctx context.Context, f domain.ListFilter, after *repository.Cursor, limit int) (service.Page[domain.Tenant, repository.Cursor], error)
	Patch(ctx context.Context, tenantID string, in domain.PatchInput) (domain.Tenant, error)
	ReplaceHostnames(ctx context.Context, tenantID string, in domain.HostnamesInput) (domain.Tenant, error)
	Suspend(ctx context.Context, tenantID, reason string) (domain.Tenant, error)
	Reinstate(ctx context.Context, tenantID string) (domain.Tenant, error)
	Delete(ctx context.Context, tenantID, confirmTenantID string) (domain.Tenant, error)
}

// NewRouter returns the service's routes on the standard httpkit chain.
// POSTs carrying an Idempotency-Key are replayed from idem.
func NewRouter(svc Tenants, idem httpkit.IdempotencyStore, build httpkit.BuildInfo, checks []httpkit.Check) http.Handler {
	r := httpkit.NewRouter(httpkit.RouterOptions{
		Build:  build,
		Checks: checks,
		Middleware: []func(http.Handler) http.Handler{
			authctx.Middleware, authctx.Audience, httpkit.LimitBody(httpkit.MaxBodyBytes),
			httpkit.Idempotency(httpkit.IdempotencyOptions{Store: idem}),
		},
	})
	strict := api.NewStrictHandlerWithOptions(handlers{svc: svc}, nil, api.StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  httpkit.RequestError,
		ResponseErrorHandlerFunc: httpkit.ResponseError,
	})
	api.HandlerWithOptions(strict, api.ChiServerOptions{BaseRouter: r, ErrorHandlerFunc: httpkit.ParamError})
	return r
}

// Routes are the gateway routes yc-tenant registers (ADR-020 rule 7).
func Routes() []httpkit.RegisteredRoute {
	return []httpkit.RegisteredRoute{
		{Prefix: "/sysadmin/tenants", Roles: []string{string(authctx.ActorSystemAdmin)}},
	}
}

type handlers struct {
	svc Tenants
}

var _ api.StrictServerInterface = handlers{}

func (h handlers) ListTenants(ctx context.Context, req api.ListTenantsRequestObject) (api.ListTenantsResponseObject, error) {
	size, after, err := page(req.Params.PageSize, req.Params.Cursor)
	if err != nil {
		return nil, err
	}
	f := domain.ListFilter{Search: deref(req.Params.Q)}
	if req.Params.Status != nil {
		f.Status = domain.Status(*req.Params.Status)
	}
	res, err := h.svc.List(ctx, f, after, size)
	if err != nil {
		return nil, err
	}
	return api.ListTenants200JSONResponse(tenantList(res, size, req.Params.Cursor)), nil
}

func (h handlers) CreateTenant(ctx context.Context, req api.CreateTenantRequestObject) (api.CreateTenantResponseObject, error) {
	b := req.Body
	t, err := h.svc.Create(ctx, domain.CreateInput{
		TenantID: b.TenantID, Name: b.Name, Hostnames: b.Hostnames, StoreCurrency: b.StoreCurrency,
		DefaultJurisdiction: b.DefaultJurisdiction, IsTaxInclusive: b.IsTaxInclusive,
		ContactEmail: string(b.ContactEmail), PrimaryAdminEmail: string(deref(b.PrimaryAdminEmail)),
	})
	if err != nil {
		return nil, err
	}
	return api.CreateTenant201JSONResponse(tenant(t)), nil
}

func (h handlers) GetTenant(ctx context.Context, req api.GetTenantRequestObject) (api.GetTenantResponseObject, error) {
	t, err := h.svc.Get(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	return api.GetTenant200JSONResponse(tenant(t)), nil
}

func (h handlers) PatchTenant(ctx context.Context, req api.PatchTenantRequestObject) (api.PatchTenantResponseObject, error) {
	in := domain.PatchInput{}
	if req.Body.Name != nil {
		in.Name = req.Body.Name
	}
	if req.Body.ContactEmail != nil {
		e := string(*req.Body.ContactEmail)
		in.ContactEmail = &e
	}
	t, err := h.svc.Patch(ctx, req.ID, in)
	if err != nil {
		return nil, err
	}
	return api.PatchTenant200JSONResponse(tenant(t)), nil
}

func (h handlers) ReplaceTenantHostnames(ctx context.Context, req api.ReplaceTenantHostnamesRequestObject) (api.ReplaceTenantHostnamesResponseObject, error) {
	t, err := h.svc.ReplaceHostnames(ctx, req.ID, domain.HostnamesInput{
		Hostnames: req.Body.Hostnames, Primary: deref(req.Body.PrimaryHostname),
	})
	if err != nil {
		return nil, err
	}
	return api.ReplaceTenantHostnames200JSONResponse(tenant(t)), nil
}

func (h handlers) SuspendTenant(ctx context.Context, req api.SuspendTenantRequestObject) (api.SuspendTenantResponseObject, error) {
	t, err := h.svc.Suspend(ctx, req.ID, req.Body.Reason)
	if err != nil {
		return nil, err
	}
	return api.SuspendTenant200JSONResponse(tenant(t)), nil
}

func (h handlers) ReinstateTenant(ctx context.Context, req api.ReinstateTenantRequestObject) (api.ReinstateTenantResponseObject, error) {
	t, err := h.svc.Reinstate(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	return api.ReinstateTenant200JSONResponse(tenant(t)), nil
}

func (h handlers) DeleteTenant(ctx context.Context, req api.DeleteTenantRequestObject) (api.DeleteTenantResponseObject, error) {
	t, err := h.svc.Delete(ctx, req.ID, req.Body.ConfirmTenantID)
	if err != nil {
		return nil, err
	}
	return api.DeleteTenant200JSONResponse(tenant(t)), nil
}

func page(size *int, cursor *string) (int, *repository.Cursor, error) {
	p, err := httpkit.ParsePage(size, cursor)
	if err != nil {
		return 0, nil, err
	}
	if p.Cursor == "" {
		return p.Size, nil, nil
	}
	after := new(repository.Cursor)
	if err := httpkit.DecodeCursor(p.Cursor, after); err != nil {
		return 0, nil, err
	}
	return p.Size, after, nil
}

func tenantList(p service.Page[domain.Tenant, repository.Cursor], size int, cursor *string) api.TenantList {
	info := httpkit.PageInfo{Size: size, Cursor: deref(cursor)}
	if p.Next != nil {
		info.NextCursor = httpkit.EncodeCursor(p.Next)
	}
	out := api.TenantList{Items: make([]api.Tenant, len(p.Items)), Page: info}
	for i, t := range p.Items {
		out.Items[i] = tenant(t)
	}
	return out
}

func tenant(t domain.Tenant) api.Tenant {
	out := api.Tenant{
		TenantID: t.TenantID, Name: t.Name, Status: api.Status(t.Status), StoreCurrency: t.StoreCurrency,
		DefaultJurisdiction: t.DefaultJurisdiction, IsTaxInclusive: t.IsTaxInclusive, ContactEmail: t.ContactEmail,
		Hostnames: t.HostnameNames(), CreatedAt: t.CreatedAt.UTC(), UpdatedAt: t.UpdatedAt.UTC(),
	}
	if p := t.PrimaryHostname(); p != "" {
		out.PrimaryHostname = &p
	}
	if t.SuspendedAt != nil {
		at := t.SuspendedAt.UTC()
		out.SuspendedAt = &at
	}
	if t.SuspendedReason != "" {
		out.SuspendedReason = &t.SuspendedReason
	}
	if t.DeletedAt != nil {
		at := t.DeletedAt.UTC()
		out.DeletedAt = &at
	}
	if t.DataPurgedAt != nil {
		at := t.DataPurgedAt.UTC()
		out.DataPurgedAt = &at
	}
	return out
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
