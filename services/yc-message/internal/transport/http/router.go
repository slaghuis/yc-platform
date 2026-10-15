// Package http is yc-message's HTTP transport. Routing, parameter parsing
// and request/response types are generated from api/openapi.yaml.
package http

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	api "github.com/slaghuis/yc-platform/services/yc-message/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-message/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-message/internal/repository"
	"github.com/slaghuis/yc-platform/services/yc-message/internal/service"
)

// Messaging is the service the handlers call.
type Messaging interface {
	Messages(ctx context.Context, f repository.MessageFilter, after *repository.MessageCursor, limit int) (service.MessagePage, error)
	Message(ctx context.Context, id string) (domain.Message, error)
	Templates(ctx context.Context) ([]domain.Template, error)
	Template(ctx context.Context, k service.TemplateKey) (domain.Template, error)
	PutTemplate(ctx context.Context, tpl domain.Template) (domain.Template, error)
	DeleteTemplate(ctx context.Context, k service.TemplateKey) error
	TestSend(ctx context.Context, k service.TemplateKey, address string) (domain.Message, error)
	Send(ctx context.Context, r service.SendRequest) (domain.Message, error)
	StartVerification(ctx context.Context, r service.VerifyRequest) (domain.Verification, error)
	ConfirmVerification(ctx context.Context, r service.VerifyRequest, code string) (domain.Verification, error)
	ReportDelivery(ctx context.Context, r service.DeliveryReport) error
}

// FakeWebhookPath is the development delivery-report webhook.
const FakeWebhookPath = "/webhooks/fake-sms"

// NewRouter returns the service's routes on the standard httpkit chain.
// POSTs carrying an Idempotency-Key are replayed from idem.
func NewRouter(svc Messaging, idem httpkit.IdempotencyStore, build httpkit.BuildInfo, checks []httpkit.Check) http.Handler {
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

// Routes are the gateway routes yc-message registers (ADR-020 rule 7).
// The fake webhook is registered only when it is enabled.
func Routes(fakeWebhook bool) []httpkit.RegisteredRoute {
	admin := []string{string(authctx.ActorShopAdmin)}
	routes := []httpkit.RegisteredRoute{
		{Prefix: "/admin/messages", Methods: []string{http.MethodGet}, Roles: admin},
		{Prefix: "/admin/templates", Roles: admin},
		{Prefix: "/internal/send", Methods: []string{http.MethodPost}},
		{Prefix: "/internal/channels", Methods: []string{http.MethodPost}},
	}
	if fakeWebhook {
		routes = append(routes, httpkit.RegisteredRoute{Prefix: FakeWebhookPath, Methods: []string{http.MethodPost}, Callback: true})
	}
	return routes
}

type handlers struct {
	svc Messaging
}

var _ api.StrictServerInterface = handlers{}

func (h handlers) ListMessages(ctx context.Context, req api.ListMessagesRequestObject) (api.ListMessagesResponseObject, error) {
	page, err := httpkit.ParsePage(req.Params.PageSize, req.Params.Cursor)
	if err != nil {
		return nil, err
	}
	var after *repository.MessageCursor
	if page.Cursor != "" {
		after = &repository.MessageCursor{}
		if err := httpkit.DecodeCursor(page.Cursor, after); err != nil {
			return nil, err
		}
	}
	var f repository.MessageFilter
	if p := req.Params; p.Status != nil {
		f.Status = domain.Status(*p.Status)
	}
	if req.Params.Type != nil {
		f.Type = domain.Type(*req.Params.Type)
	}
	if req.Params.UserID != nil {
		f.UserID = req.Params.UserID.String()
	}
	res, err := h.svc.Messages(ctx, f, after, page.Size)
	if err != nil {
		return nil, err
	}
	info := httpkit.PageInfo{Size: page.Size, Cursor: page.Cursor}
	if res.Next != nil {
		info.NextCursor = httpkit.EncodeCursor(res.Next)
	}
	out := api.ListMessages200JSONResponse{Items: make([]api.Message, len(res.Messages)), Page: info}
	for i, m := range res.Messages {
		out.Items[i] = message(m)
	}
	return out, nil
}

func (h handlers) GetMessage(ctx context.Context, req api.GetMessageRequestObject) (api.GetMessageResponseObject, error) {
	m, err := h.svc.Message(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.GetMessage200JSONResponse(message(m)), nil
}

func (h handlers) ListTemplates(ctx context.Context, _ api.ListTemplatesRequestObject) (api.ListTemplatesResponseObject, error) {
	tpls, err := h.svc.Templates(ctx)
	if err != nil {
		return nil, err
	}
	out := api.ListTemplates200JSONResponse{Items: make([]api.Template, len(tpls))}
	for i, t := range tpls {
		out.Items[i] = template(t)
	}
	return out, nil
}

func (h handlers) GetTemplate(ctx context.Context, req api.GetTemplateRequestObject) (api.GetTemplateResponseObject, error) {
	t, err := h.svc.Template(ctx, templateKey(req.Type, req.Channel, req.Locale))
	if err != nil {
		return nil, err
	}
	return api.GetTemplate200JSONResponse(template(t)), nil
}

func (h handlers) PutTemplate(ctx context.Context, req api.PutTemplateRequestObject) (api.PutTemplateResponseObject, error) {
	k := templateKey(req.Type, req.Channel, req.Locale)
	b := req.Body
	tpl := domain.Template{Type: k.Type, Channel: k.Channel, Locale: k.Locale, Subject: deref(b.Subject), Body: b.Body}
	if b.Format != nil {
		tpl.Format = domain.Format(*b.Format)
	}
	t, err := h.svc.PutTemplate(ctx, tpl)
	if err != nil {
		return nil, err
	}
	return api.PutTemplate200JSONResponse(template(t)), nil
}

func (h handlers) DeleteTemplate(ctx context.Context, req api.DeleteTemplateRequestObject) (api.DeleteTemplateResponseObject, error) {
	if err := h.svc.DeleteTemplate(ctx, templateKey(req.Type, req.Channel, req.Locale)); err != nil {
		return nil, err
	}
	return api.DeleteTemplate204Response{}, nil
}

func (h handlers) TestTemplate(ctx context.Context, req api.TestTemplateRequestObject) (api.TestTemplateResponseObject, error) {
	m, err := h.svc.TestSend(ctx, templateKey(req.Type, req.Channel, req.Locale), req.Body.Address)
	if err != nil {
		return nil, err
	}
	return api.TestTemplate202JSONResponse(message(m)), nil
}

func (h handlers) SendMessage(ctx context.Context, req api.SendMessageRequestObject) (api.SendMessageResponseObject, error) {
	b := req.Body
	r := service.SendRequest{
		Type: domain.Type(b.Type), Reference: b.Reference, Address: deref(b.Address), Locale: deref(b.Locale),
		Context: deref(b.Context),
	}
	if b.UserID != nil {
		r.UserID = b.UserID.String()
	}
	if b.Channel != nil {
		r.Channel = domain.Channel(*b.Channel)
	}
	m, err := h.svc.Send(ctx, r)
	if err != nil {
		return nil, err
	}
	return api.SendMessage202JSONResponse{
		MessageID: mustUUID(m.ID), Type: api.MessageType(m.Type), Channel: api.Channel(m.Channel), Status: api.MessageStatus(m.Status),
	}, nil
}

func (h handlers) StartVerification(ctx context.Context, req api.StartVerificationRequestObject) (api.StartVerificationResponseObject, error) {
	b := req.Body
	v, err := h.svc.StartVerification(ctx, service.VerifyRequest{
		UserID: b.UserID.String(), Channel: domain.Channel(b.Channel), Address: b.Address,
	})
	if err != nil {
		return nil, err
	}
	return api.StartVerification202JSONResponse(verification(v)), nil
}

func (h handlers) ConfirmVerification(ctx context.Context, req api.ConfirmVerificationRequestObject) (api.ConfirmVerificationResponseObject, error) {
	b := req.Body
	v, err := h.svc.ConfirmVerification(ctx, service.VerifyRequest{
		UserID: b.UserID.String(), Channel: domain.Channel(b.Channel), Address: b.Address,
	}, b.Code)
	if err != nil {
		return nil, err
	}
	return api.ConfirmVerification200JSONResponse(verification(v)), nil
}

func (h handlers) FakeDeliveryReport(ctx context.Context, req api.FakeDeliveryReportRequestObject) (api.FakeDeliveryReportResponseObject, error) {
	b := req.Body
	err := h.svc.ReportDelivery(ctx, service.DeliveryReport{
		ProviderMessageID: b.ProviderMessageID, Status: domain.Status(b.Status), Reason: deref(b.Reason),
	})
	if err != nil {
		return nil, err
	}
	return api.FakeDeliveryReport204Response{}, nil
}

func templateKey(t api.TemplateTypeParam, ch api.TemplateChannelParam, locale api.Locale) service.TemplateKey {
	return service.TemplateKey{Type: domain.Type(t), Channel: domain.Channel(ch), Locale: locale}
}

func message(m domain.Message) api.Message {
	out := api.Message{
		MessageID: mustUUID(m.ID), Type: api.MessageType(m.Type), Channel: api.Channel(m.Channel), Address: m.Address,
		Locale: m.Locale, Subject: optional(m.Subject), Body: m.Body, Format: api.Format(m.Format),
		Status: api.MessageStatus(m.Status), Driver: m.Driver, ProviderMessageID: optional(m.ProviderMessageID),
		Attempts: m.Attempts, LastError: optional(m.LastError), NextAttemptAt: utc(m.NextAttemptAt),
		Reference: m.Reference, CreatedAt: m.CreatedAt.UTC(), UpdatedAt: m.UpdatedAt.UTC(),
		SentAt: utc(m.SentAt), DeliveredAt: utc(m.DeliveredAt),
	}
	if m.UserID != "" {
		id := mustUUID(m.UserID)
		out.UserID = &id
	}
	return out
}

func template(t domain.Template) api.Template {
	out := api.Template{
		Type: api.MessageType(t.Type), Channel: api.TemplateChannel(t.Channel), Locale: t.Locale,
		Subject: optional(t.Subject), Body: t.Body, Format: api.Format(t.Format), Platform: t.Platform, Version: t.Version,
	}
	if !t.UpdatedAt.IsZero() {
		at := t.UpdatedAt.UTC()
		out.UpdatedAt = &at
	}
	return out
}

func verification(v domain.Verification) api.Verification {
	return api.Verification{
		VerificationID: mustUUID(v.ID), Channel: string(v.Channel), Address: v.Address,
		ExpiresAt: v.ExpiresAt.UTC(), ConfirmedAt: utc(v.ConfirmedAt),
	}
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
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

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
