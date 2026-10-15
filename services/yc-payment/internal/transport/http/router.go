// Package http is yc-payment's HTTP transport. Routing, parameter parsing
// and request/response types are generated from api/openapi.yaml, except
// the webhook, which must see the raw body to check its signature.
package http

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"

	api "github.com/slaghuis/yc-platform/services/yc-payment/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-payment/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-payment/internal/gateway"
	"github.com/slaghuis/yc-platform/services/yc-payment/internal/repository"
	"github.com/slaghuis/yc-platform/services/yc-payment/internal/service"
)

// Payments is the service the handlers call.
type Payments interface {
	GatewayConfigs(ctx context.Context) ([]domain.GatewayConfig, error)
	PutGatewayConfig(ctx context.Context, gw string, in service.PutGatewayConfig) (domain.GatewayConfig, error)
	TestGateway(ctx context.Context, gw string) error
	Capabilities(ctx context.Context, gw string) (service.Capabilities, error)
	Route(ctx context.Context, currency, gw string) (service.Capabilities, error)
	Authorize(ctx context.Context, req service.PaymentRequest) (domain.Transaction, error)
	Charge(ctx context.Context, req service.PaymentRequest) (domain.Transaction, error)
	Capture(ctx context.Context, req service.CaptureRequest) (domain.Transaction, error)
	Void(ctx context.Context, req service.VoidRequest) (domain.Transaction, bool, error)
	Refund(ctx context.Context, req service.RefundRequest) (domain.Refund, bool, error)
	Transaction(ctx context.Context, id string) (domain.Transaction, []domain.Refund, error)
	ListTransactions(ctx context.Context, f repository.TransactionFilter, after *repository.TransactionCursor, limit int) ([]domain.Transaction, error)
	Webhook(ctx context.Context, gw string, header http.Header, body []byte) error
}

// Webhook paths, on the shop's host.
const (
	WebhookPath     = "/webhooks/stripe"
	YocoWebhookPath = "/webhooks/yoco"
)

// NewRouter returns the service's routes on the standard httpkit chain.
func NewRouter(svc Payments, build httpkit.BuildInfo, checks []httpkit.Check) http.Handler {
	r := httpkit.NewRouter(httpkit.RouterOptions{
		Build:  build,
		Checks: checks,
		Middleware: []func(http.Handler) http.Handler{
			authctx.Middleware, authctx.Audience, httpkit.LimitBody(httpkit.MaxBodyBytes),
		},
	})
	r.Post(WebhookPath, webhook(svc, "stripe"))
	r.Post(YocoWebhookPath, webhook(svc, "yoco"))
	strict := api.NewStrictHandlerWithOptions(handlers{svc: svc}, nil, api.StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  httpkit.RequestError,
		ResponseErrorHandlerFunc: httpkit.ResponseError,
	})
	api.HandlerWithOptions(strict, api.ChiServerOptions{BaseRouter: r, ErrorHandlerFunc: httpkit.ParamError})
	return r
}

// Routes are the gateway routes yc-payment registers (ADR-020 rule 7).
// Webhooks are callback routes: no token, verified by their signature.
func Routes() []httpkit.RegisteredRoute {
	admin := []string{string(authctx.ActorShopAdmin)}
	return []httpkit.RegisteredRoute{
		{Prefix: "/admin/gateway-config", Roles: admin},
		{Prefix: "/admin/transactions", Methods: []string{http.MethodGet}, Roles: admin},
		{Prefix: WebhookPath, Methods: []string{http.MethodPost}, Callback: true},
		{Prefix: YocoWebhookPath, Methods: []string{http.MethodPost}, Callback: true},
		{Prefix: "/internal/authorize", Methods: []string{http.MethodPost}},
		{Prefix: "/internal/charge", Methods: []string{http.MethodPost}},
		{Prefix: "/internal/gateway-route", Methods: []string{http.MethodGet}},
		{Prefix: "/internal/capture", Methods: []string{http.MethodPost}},
		{Prefix: "/internal/void", Methods: []string{http.MethodPost}},
		{Prefix: "/internal/refund", Methods: []string{http.MethodPost}},
		{Prefix: "/internal/transactions", Methods: []string{http.MethodGet}},
		{Prefix: "/internal/gateway-capabilities", Methods: []string{http.MethodGet}},
	}
}

func webhook(svc Payments, gw string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			ycerrors.WriteError(w, r, ycerrors.Validation().Message("Unreadable request body").Wrap(err))
			return
		}
		if err := svc.Webhook(r.Context(), gw, r.Header, body); err != nil {
			ycerrors.WriteError(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"received":true}`))
	}
}

type handlers struct {
	svc Payments
}

var _ api.StrictServerInterface = handlers{}

func (h handlers) ListGatewayConfigs(ctx context.Context, _ api.ListGatewayConfigsRequestObject) (api.ListGatewayConfigsResponseObject, error) {
	cs, err := h.svc.GatewayConfigs(ctx)
	if err != nil {
		return nil, err
	}
	out := api.ListGatewayConfigs200JSONResponse{Items: make([]api.GatewayConfig, len(cs))}
	for i, c := range cs {
		out.Items[i] = gatewayConfig(c)
	}
	return out, nil
}

func (h handlers) PutGatewayConfig(ctx context.Context, req api.PutGatewayConfigRequestObject) (api.PutGatewayConfigResponseObject, error) {
	c, err := h.svc.PutGatewayConfig(ctx, string(req.Gateway), service.PutGatewayConfig{
		Enabled: req.Body.Enabled, APIKey: deref(req.Body.APIKey), WebhookSecret: req.Body.WebhookSecret,
		PublicKey: deref(req.Body.PublicKey), Priority: req.Body.Priority, Currencies: req.Body.Currencies,
	})
	if err != nil {
		return nil, err
	}
	return api.PutGatewayConfig200JSONResponse(gatewayConfig(c)), nil
}

func (h handlers) TestGatewayConfig(ctx context.Context, req api.TestGatewayConfigRequestObject) (api.TestGatewayConfigResponseObject, error) {
	err := h.svc.TestGateway(ctx, string(req.Gateway))
	var de *gateway.DeclineError
	if errors.As(err, &de) {
		msg := de.Code
		if de.Message != "" {
			msg += ": " + de.Message
		}
		return api.TestGatewayConfig200JSONResponse{Ok: false, Message: &msg}, nil
	}
	if err != nil {
		return nil, err
	}
	return api.TestGatewayConfig200JSONResponse{Ok: true}, nil
}

func (h handlers) GetGatewayCapabilities(ctx context.Context, req api.GetGatewayCapabilitiesRequestObject) (api.GetGatewayCapabilitiesResponseObject, error) {
	c, err := h.svc.Capabilities(ctx, string(req.Gateway))
	if err != nil {
		return nil, err
	}
	return api.GetGatewayCapabilities200JSONResponse(capabilities(c)), nil
}

func (h handlers) RouteGateway(ctx context.Context, req api.RouteGatewayRequestObject) (api.RouteGatewayResponseObject, error) {
	gw := ""
	if req.Params.Gateway != nil {
		gw = string(*req.Params.Gateway)
	}
	c, err := h.svc.Route(ctx, req.Params.Currency, gw)
	if err != nil {
		return nil, err
	}
	return api.RouteGateway200JSONResponse(capabilities(c)), nil
}

func capabilities(c service.Capabilities) api.Capabilities {
	out := api.Capabilities{
		Gateway: c.Gateway, SupportsSeparateAuthCapture: c.SupportsSeparateAuthCapture, HostedPayment: c.HostedPayment,
		Configured: c.Configured, PublicKey: optional(c.PublicKey),
	}
	if c.SupportedCurrencies != nil {
		cur := c.SupportedCurrencies
		out.SupportedCurrencies = &cur
	}
	return out
}

func (h handlers) ListTransactions(ctx context.Context, req api.ListTransactionsRequestObject) (api.ListTransactionsResponseObject, error) {
	page, err := httpkit.ParsePage(req.Params.PageSize, req.Params.Cursor)
	if err != nil {
		return nil, err
	}
	var after *repository.TransactionCursor
	if page.Cursor != "" {
		after = &repository.TransactionCursor{}
		if err := httpkit.DecodeCursor(page.Cursor, after); err != nil {
			return nil, err
		}
	}
	f := repository.TransactionFilter{SessionID: deref(req.Params.SessionID)}
	if req.Params.Status != nil {
		f.Status = string(*req.Params.Status)
	}
	if req.Params.OrderID != nil {
		f.OrderID = req.Params.OrderID.String()
	}
	ts, err := h.svc.ListTransactions(ctx, f, after, page.Size+1)
	if err != nil {
		return nil, err
	}
	info := httpkit.PageInfo{Size: page.Size, Cursor: page.Cursor}
	if len(ts) > page.Size {
		ts = ts[:page.Size]
		last := ts[len(ts)-1]
		info.NextCursor = httpkit.EncodeCursor(repository.TransactionCursor{At: last.CreatedAt, ID: last.ID})
	}
	out := api.ListTransactions200JSONResponse{Items: make([]api.Transaction, len(ts)), Page: info}
	for i, t := range ts {
		out.Items[i] = transaction(t)
	}
	return out, nil
}

func (h handlers) GetTransaction(ctx context.Context, req api.GetTransactionRequestObject) (api.GetTransactionResponseObject, error) {
	t, refunds, err := h.svc.Transaction(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.GetTransaction200JSONResponse(detail(t, refunds)), nil
}

func (h handlers) GetInternalTransaction(ctx context.Context, req api.GetInternalTransactionRequestObject) (api.GetInternalTransactionResponseObject, error) {
	t, refunds, err := h.svc.Transaction(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.GetInternalTransaction200JSONResponse(detail(t, refunds)), nil
}

func (h handlers) AuthorizePayment(ctx context.Context, req api.AuthorizePaymentRequestObject) (api.AuthorizePaymentResponseObject, error) {
	gw := "stripe"
	if req.Body.Gateway != nil {
		gw = string(*req.Body.Gateway)
	}
	t, err := h.svc.Authorize(ctx, service.PaymentRequest{
		SessionID: req.Body.SessionID, Gateway: gw, AmountMinor: req.Body.Amount.AmountMinor,
		Currency: req.Body.Amount.Currency, PaymentToken: req.Body.PaymentToken, ReturnURL: deref(req.Body.ReturnURL),
		IdempotencyKey: req.Params.IdempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	return api.AuthorizePayment200JSONResponse(transaction(t)), nil
}

func (h handlers) ChargePayment(ctx context.Context, req api.ChargePaymentRequestObject) (api.ChargePaymentResponseObject, error) {
	in := service.PaymentRequest{
		SessionID: req.Body.SessionID, Gateway: string(req.Body.Gateway), AmountMinor: req.Body.Amount.AmountMinor,
		Currency: req.Body.Amount.Currency, PaymentToken: deref(req.Body.PaymentToken), ReturnURL: deref(req.Body.ReturnURL),
		IdempotencyKey: req.Params.IdempotencyKey,
	}
	if req.Body.OrderID != nil {
		in.OrderID = req.Body.OrderID.String()
	}
	t, err := h.svc.Charge(ctx, in)
	if err != nil {
		return nil, err
	}
	return api.ChargePayment200JSONResponse(transaction(t)), nil
}

func (h handlers) CapturePayment(ctx context.Context, req api.CapturePaymentRequestObject) (api.CapturePaymentResponseObject, error) {
	in := service.CaptureRequest{TransactionID: req.Body.TransactionID.String(), AmountMinor: deref(req.Body.AmountMinor)}
	if req.Body.OrderID != nil {
		in.OrderID = req.Body.OrderID.String()
	}
	t, err := h.svc.Capture(ctx, in)
	if err != nil {
		return nil, err
	}
	return api.CapturePayment200JSONResponse(transaction(t)), nil
}

func (h handlers) VoidPayment(ctx context.Context, req api.VoidPaymentRequestObject) (api.VoidPaymentResponseObject, error) {
	in := service.VoidRequest{AuthorizationKey: deref(req.Body.AuthorizationKey)}
	if req.Body.TransactionID != nil {
		in.TransactionID = req.Body.TransactionID.String()
	}
	t, found, err := h.svc.Void(ctx, in)
	if err != nil {
		return nil, err
	}
	if !found {
		return api.VoidPayment200JSONResponse{Status: "none"}, nil
	}
	id, _ := uuid.Parse(t.ID)
	return api.VoidPayment200JSONResponse{Status: api.VoidResultStatus(t.Status), TransactionID: &id}, nil
}

func (h handlers) RefundPayment(ctx context.Context, req api.RefundPaymentRequestObject) (api.RefundPaymentResponseObject, error) {
	in := service.RefundRequest{
		TransactionID: req.Body.TransactionID.String(), AmountMinor: deref(req.Body.AmountMinor),
		Reason: deref(req.Body.Reason), IdempotencyKey: req.Params.IdempotencyKey,
	}
	if req.Body.OrderID != nil {
		in.OrderID = req.Body.OrderID.String()
	}
	r, found, err := h.svc.Refund(ctx, in)
	if err != nil {
		return nil, err
	}
	if !found {
		return api.RefundPayment200JSONResponse{Status: "none"}, nil
	}
	out := refund(r)
	return api.RefundPayment200JSONResponse{Status: api.RefundResultStatus(r.Status), Refund: &out}, nil
}

func gatewayConfig(c domain.GatewayConfig) api.GatewayConfig {
	currencies := c.Currencies
	if currencies == nil {
		currencies = []string{}
	}
	return api.GatewayConfig{
		Gateway: c.Gateway, Enabled: c.Enabled, APIKeyHint: c.APIKeyHint, WebhookSecretSet: c.WebhookSecretSet,
		PublicKey: optional(c.PublicKey), Priority: c.Priority, Currencies: currencies, UpdatedAt: c.UpdatedAt.UTC(),
	}
}

func transaction(t domain.Transaction) api.Transaction {
	id, _ := uuid.Parse(t.ID)
	kind := api.TransactionKind(t.Kind)
	out := api.Transaction{
		TransactionID: id, SessionID: t.SessionID, Kind: &kind, Gateway: t.Gateway, GatewayReference: optional(t.GatewayReference),
		Status: api.TransactionStatus(t.Status), Amount: api.Money{AmountMinor: t.AmountMinor, Currency: t.Currency},
		CapturedAmountMinor: t.CapturedMinor, RefundedAmountMinor: t.RefundedMinor,
		FailureReason: optional(t.FailureReason), CreatedAt: t.CreatedAt.UTC(), UpdatedAt: t.UpdatedAt.UTC(),
	}
	if t.OrderID != "" {
		o, _ := uuid.Parse(t.OrderID)
		out.OrderID = &o
	}
	if t.NextAction != nil {
		next := t.NextAction
		out.NextAction = &next
	}
	return out
}

func detail(t domain.Transaction, refunds []domain.Refund) api.TransactionDetail {
	x := transaction(t)
	out := api.TransactionDetail{
		TransactionID: x.TransactionID, SessionID: x.SessionID, OrderID: x.OrderID,
		Kind: (*api.TransactionDetailKind)(x.Kind), Gateway: x.Gateway,
		GatewayReference: x.GatewayReference, Status: x.Status, Amount: x.Amount,
		CapturedAmountMinor: x.CapturedAmountMinor, RefundedAmountMinor: x.RefundedAmountMinor,
		NextAction: x.NextAction, FailureReason: x.FailureReason, CreatedAt: x.CreatedAt, UpdatedAt: x.UpdatedAt,
		Refunds: make([]api.Refund, len(refunds)),
	}
	for i, r := range refunds {
		out.Refunds[i] = refund(r)
	}
	return out
}

func refund(r domain.Refund) api.Refund {
	id, _ := uuid.Parse(r.ID)
	tid, _ := uuid.Parse(r.TransactionID)
	out := api.Refund{
		RefundID: id, TransactionID: tid, AmountMinor: r.AmountMinor, Reason: optional(r.Reason),
		GatewayReference: optional(r.GatewayReference), Status: api.RefundStatus(r.Status),
		FailureReason: optional(r.FailureReason), CreatedAt: r.CreatedAt.UTC(),
	}
	if r.OrderID != "" {
		o, _ := uuid.Parse(r.OrderID)
		out.OrderID = &o
	}
	if r.CompletedAt != nil {
		at := r.CompletedAt.UTC()
		out.CompletedAt = &at
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
