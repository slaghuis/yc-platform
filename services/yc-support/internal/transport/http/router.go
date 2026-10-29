// Package http is yc-support's HTTP transport. Routing, parameter parsing
// and request/response types are generated from api/openapi.yaml.
package http

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	api "github.com/slaghuis/yc-platform/services/yc-support/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-support/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-support/internal/repository"
	"github.com/slaghuis/yc-platform/services/yc-support/internal/service"
)

// Support is the service the handlers call.
type Support interface {
	Create(ctx context.Context, in domain.NewRequest) (domain.Thread, error)
	MyRequests(ctx context.Context, after *repository.Cursor, limit int) (service.Page[domain.Request], error)
	MyRequest(ctx context.Context, id string) (domain.Thread, error)
	Reply(ctx context.Context, id, body string) (domain.Message, error)
	AdminRequests(ctx context.Context, f service.AdminFilter, after *repository.Cursor, limit int) (service.Page[domain.Request], error)
	AdminRequest(ctx context.Context, id string) (domain.Thread, error)
	AdminReply(ctx context.Context, id, body string) (domain.Message, error)
	CloseRequest(ctx context.Context, id string) (domain.Request, error)
	Claim(ctx context.Context, userID, email string) (int64, error)
}

// NewRouter returns the service's routes on the standard httpkit chain.
func NewRouter(svc Support, build httpkit.BuildInfo, checks []httpkit.Check) http.Handler {
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

// Routes are the gateway routes yc-support registers (ADR-020 rule 7).
// Customers and visitors may open requests; the service itself restricts
// listing, reading and replying to customers.
func Routes() []httpkit.RegisteredRoute {
	return []httpkit.RegisteredRoute{
		{Prefix: "/support/requests", Roles: []string{string(authctx.ActorCustomer), string(authctx.ActorVisitor)}},
		{Prefix: "/admin/support", Roles: []string{string(authctx.ActorShopAdmin)}},
		{Prefix: "/internal/support/requests/claim", Methods: []string{http.MethodPost}},
	}
}

type handlers struct {
	svc Support
}

var _ api.StrictServerInterface = handlers{}

func (h handlers) CreateSupportRequest(ctx context.Context, req api.CreateSupportRequestRequestObject) (api.CreateSupportRequestResponseObject, error) {
	b := req.Body
	in := domain.NewRequest{
		Topic: domain.Topic(b.Topic), ContactEmail: b.ContactEmail, ContactName: b.ContactName,
		ContactPhone: deref(b.ContactPhone), PageURL: deref(b.PageURL), Body: b.Body,
	}
	if b.OrderID != nil {
		in.OrderID = b.OrderID.String()
	}
	t, err := h.svc.Create(ctx, in)
	if err != nil {
		return nil, err
	}
	return api.CreateSupportRequest201JSONResponse(thread(t)), nil
}

func (h handlers) ListMySupportRequests(ctx context.Context, req api.ListMySupportRequestsRequestObject) (api.ListMySupportRequestsResponseObject, error) {
	size, after, err := pageParams(req.Params.PageSize, req.Params.Cursor)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.MyRequests(ctx, after, size)
	if err != nil {
		return nil, err
	}
	return api.ListMySupportRequests200JSONResponse(requestList(p, size, req.Params.Cursor)), nil
}

func (h handlers) GetMySupportRequest(ctx context.Context, req api.GetMySupportRequestRequestObject) (api.GetMySupportRequestResponseObject, error) {
	t, err := h.svc.MyRequest(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.GetMySupportRequest200JSONResponse(thread(t)), nil
}

func (h handlers) ReplyToMySupportRequest(ctx context.Context, req api.ReplyToMySupportRequestRequestObject) (api.ReplyToMySupportRequestResponseObject, error) {
	m, err := h.svc.Reply(ctx, req.ID.String(), req.Body.Body)
	if err != nil {
		return nil, err
	}
	return api.ReplyToMySupportRequest201JSONResponse(message(m)), nil
}

func (h handlers) AdminListSupportRequests(ctx context.Context, req api.AdminListSupportRequestsRequestObject) (api.AdminListSupportRequestsResponseObject, error) {
	size, after, err := pageParams(req.Params.PageSize, req.Params.Cursor)
	if err != nil {
		return nil, err
	}
	f := service.AdminFilter{Email: deref(req.Params.Email), Reference: deref(req.Params.Reference)}
	if req.Params.Status != nil {
		f.Status = domain.Status(*req.Params.Status)
	}
	if req.Params.Topic != nil {
		f.Topic = domain.Topic(*req.Params.Topic)
	}
	p, err := h.svc.AdminRequests(ctx, f, after, size)
	if err != nil {
		return nil, err
	}
	return api.AdminListSupportRequests200JSONResponse(requestList(p, size, req.Params.Cursor)), nil
}

func (h handlers) AdminGetSupportRequest(ctx context.Context, req api.AdminGetSupportRequestRequestObject) (api.AdminGetSupportRequestResponseObject, error) {
	t, err := h.svc.AdminRequest(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.AdminGetSupportRequest200JSONResponse(thread(t)), nil
}

func (h handlers) AdminReplyToSupportRequest(ctx context.Context, req api.AdminReplyToSupportRequestRequestObject) (api.AdminReplyToSupportRequestResponseObject, error) {
	m, err := h.svc.AdminReply(ctx, req.ID.String(), req.Body.Body)
	if err != nil {
		return nil, err
	}
	return api.AdminReplyToSupportRequest201JSONResponse(message(m)), nil
}

func (h handlers) AdminCloseSupportRequest(ctx context.Context, req api.AdminCloseSupportRequestRequestObject) (api.AdminCloseSupportRequestResponseObject, error) {
	r, err := h.svc.CloseRequest(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.AdminCloseSupportRequest200JSONResponse(supportRequest(r)), nil
}

func (h handlers) ClaimSupportRequests(ctx context.Context, req api.ClaimSupportRequestsRequestObject) (api.ClaimSupportRequestsResponseObject, error) {
	n, err := h.svc.Claim(ctx, req.Body.UserID.String(), req.Body.Email)
	if err != nil {
		return nil, err
	}
	return api.ClaimSupportRequests200JSONResponse{Claimed: n}, nil
}

func pageParams(size *int, cursor *string) (int, *repository.Cursor, error) {
	p, err := httpkit.ParsePage(size, cursor)
	if err != nil {
		return 0, nil, err
	}
	if p.Cursor == "" {
		return p.Size, nil, nil
	}
	after := &repository.Cursor{}
	if err := httpkit.DecodeCursor(p.Cursor, after); err != nil {
		return 0, nil, err
	}
	return p.Size, after, nil
}

func requestList(p service.Page[domain.Request], size int, cursor *string) api.RequestList {
	info := httpkit.PageInfo{Size: size, Cursor: deref(cursor)}
	if p.Next != nil {
		info.NextCursor = httpkit.EncodeCursor(p.Next)
	}
	out := api.RequestList{Items: make([]api.SupportRequest, len(p.Items)), Page: info}
	for i, r := range p.Items {
		out.Items[i] = supportRequest(r)
	}
	return out
}

func thread(t domain.Thread) api.Thread {
	out := api.Thread{Request: supportRequest(t.Request), Messages: make([]api.Message, len(t.Messages))}
	for i, m := range t.Messages {
		out.Messages[i] = message(m)
	}
	return out
}

func supportRequest(r domain.Request) api.SupportRequest {
	out := api.SupportRequest{
		ID: uuidOf(r.ID), Reference: r.Reference, Topic: api.Topic(r.Topic), Status: api.Status(r.Status),
		ContactEmail: r.ContactEmail, ContactName: r.ContactName, ContactPhone: optional(r.ContactPhone),
		UserID: optionalUUID(r.UserID), UserType: api.UserType(r.UserType),
		OrderID: optionalUUID(r.OrderID), PageURL: optional(r.PageURL),
		LastActivityAt: r.LastActivityAt.UTC(), CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC(),
	}
	if r.ClosedAt != nil {
		t := r.ClosedAt.UTC()
		out.ClosedAt = &t
	}
	if r.ClosedReason != "" {
		c := api.ClosedReason(r.ClosedReason)
		out.ClosedReason = &c
	}
	return out
}

func message(m domain.Message) api.Message {
	return api.Message{
		ID: uuidOf(m.ID), RequestID: uuidOf(m.RequestID), AuthorType: api.AuthorType(m.AuthorType),
		AuthorUserID: optionalUUID(m.AuthorUserID), Body: m.Body, CreatedAt: m.CreatedAt.UTC(),
	}
}

func uuidOf(s string) openapi_types.UUID {
	id, _ := uuid.Parse(s)
	return id
}

func optionalUUID(s string) *openapi_types.UUID {
	if s == "" {
		return nil
	}
	id := uuidOf(s)
	return &id
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
