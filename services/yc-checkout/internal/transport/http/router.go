// Package http is yc-checkout's HTTP transport. Routing, parameter
// parsing and request/response types are generated from api/openapi.yaml.
package http

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	money "github.com/slaghuis/yc-platform/libs/yc-money"

	api "github.com/slaghuis/yc-platform/services/yc-checkout/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-checkout/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-checkout/internal/repository"
)

// Checkout is the service the handlers call.
type Checkout interface {
	Start(ctx context.Context, cartType string) (domain.Session, error)
	Session(ctx context.Context, id string) (domain.Session, error)
	SelectShipping(ctx context.Context, id string, in domain.ShippingChoice) (domain.Session, error)
	Confirm(ctx context.Context, id, key string, in domain.ConfirmInput) (domain.Session, error)
	Cancel(ctx context.Context, id string) (domain.Session, error)
	AdminSessions(ctx context.Context, state domain.State, after *repository.SessionCursor, limit int) ([]domain.Session, error)
	AdminSession(ctx context.Context, id string) (domain.Session, []repository.Event, error)
	InternalSession(ctx context.Context, id string) (domain.Session, error)
}

// NewRouter returns the service's routes on the standard httpkit chain.
// Confirm keeps its Idempotency-Key on the session, so no replay store is
// needed: a repeated key resumes the saga rather than replaying a reply.
func NewRouter(svc Checkout, build httpkit.BuildInfo, checks []httpkit.Check) http.Handler {
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

// Routes are the gateway routes yc-checkout registers (ADR-020 rule 7).
func Routes() []httpkit.RegisteredRoute {
	return []httpkit.RegisteredRoute{
		{
			Prefix: "/checkout", Methods: []string{http.MethodGet, http.MethodPost},
			Roles: []string{string(authctx.ActorCustomer), string(authctx.ActorVisitor)},
		},
		{Prefix: "/admin/sessions", Methods: []string{http.MethodGet}, Roles: []string{string(authctx.ActorShopAdmin)}},
		{Prefix: "/internal/sessions", Methods: []string{http.MethodGet}},
	}
}

type handlers struct {
	svc Checkout
}

var _ api.StrictServerInterface = handlers{}

func (h handlers) StartCheckout(ctx context.Context, req api.StartCheckoutRequestObject) (api.StartCheckoutResponseObject, error) {
	var cartType string
	if req.Body != nil {
		cartType = deref(req.Body.CartType)
	}
	s, err := h.svc.Start(ctx, cartType)
	if err != nil {
		return nil, err
	}
	return api.StartCheckout201JSONResponse(session(s, false)), nil
}

func (h handlers) GetCheckout(ctx context.Context, req api.GetCheckoutRequestObject) (api.GetCheckoutResponseObject, error) {
	s, err := h.svc.Session(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.GetCheckout200JSONResponse(session(s, false)), nil
}

func (h handlers) SelectShipping(ctx context.Context, req api.SelectShippingRequestObject) (api.SelectShippingResponseObject, error) {
	var in domain.ShippingChoice
	if b := req.Body; b != nil {
		if b.DeliveryAddress != nil {
			in.DeliveryAddress = *b.DeliveryAddress
		}
		if b.ShippingOptionID != nil {
			in.OptionID = *b.ShippingOptionID
		}
	}
	s, err := h.svc.SelectShipping(ctx, req.ID.String(), in)
	if err != nil {
		return nil, err
	}
	return api.SelectShipping200JSONResponse(session(s, false)), nil
}

func (h handlers) ConfirmCheckout(ctx context.Context, req api.ConfirmCheckoutRequestObject) (api.ConfirmCheckoutResponseObject, error) {
	b := req.Body
	in := domain.ConfirmInput{
		ShippingOptionID: deref(b.ShippingOptionID), PaymentGateway: deref(b.PaymentGateway),
		PaymentToken: deref(b.PaymentGatewayToken), ReturnURL: deref(b.ReturnURL), DeliveryAddress: deref(b.DeliveryAddress),
	}
	if g := b.GuestContact; g != nil {
		in.GuestContact = &domain.GuestContact{Email: g.Email, PhoneE164: g.PhoneE164}
	}
	s, err := h.svc.Confirm(ctx, req.ID.String(), req.Params.IdempotencyKey, in)
	if err != nil {
		return nil, err
	}
	out := session(s, false)
	switch s.State {
	case domain.StateCompleted:
		return api.ConfirmCheckout200JSONResponse(out), nil
	case domain.StateFailed, domain.StateExpired:
		return api.ConfirmCheckout422JSONResponse(out), nil
	case domain.StatePricing, domain.StateReserving, domain.StateAuthorizing, domain.StateOrderCreate,
		domain.StateCharging, domain.StateCapturing, domain.StateConfirming, domain.StateCompensating:
	}
	return api.ConfirmCheckout202JSONResponse(out), nil
}

func (h handlers) CancelCheckout(ctx context.Context, req api.CancelCheckoutRequestObject) (api.CancelCheckoutResponseObject, error) {
	s, err := h.svc.Cancel(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.CancelCheckout200JSONResponse(session(s, false)), nil
}

func (h handlers) ListSessions(ctx context.Context, req api.ListSessionsRequestObject) (api.ListSessionsResponseObject, error) {
	page, err := httpkit.ParsePage(req.Params.PageSize, req.Params.Cursor)
	if err != nil {
		return nil, err
	}
	var after *repository.SessionCursor
	if page.Cursor != "" {
		after = &repository.SessionCursor{}
		if err := httpkit.DecodeCursor(page.Cursor, after); err != nil {
			return nil, err
		}
	}
	var state domain.State
	if req.Params.State != nil {
		state = domain.State(*req.Params.State)
	}
	ss, err := h.svc.AdminSessions(ctx, state, after, page.Size+1)
	if err != nil {
		return nil, err
	}
	info := httpkit.PageInfo{Size: page.Size, Cursor: page.Cursor}
	if len(ss) > page.Size {
		ss = ss[:page.Size]
		last := ss[len(ss)-1]
		info.NextCursor = httpkit.EncodeCursor(repository.SessionCursor{CreatedAt: last.CreatedAt, ID: last.ID})
	}
	items := make([]api.AdminSession, len(ss))
	for i, s := range ss {
		items[i] = adminSession(s, nil)
	}
	return api.ListSessions200JSONResponse{Items: items, Page: info}, nil
}

func (h handlers) GetSession(ctx context.Context, req api.GetSessionRequestObject) (api.GetSessionResponseObject, error) {
	s, history, err := h.svc.AdminSession(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.GetSession200JSONResponse(adminSession(s, history)), nil
}

func (h handlers) GetInternalSession(ctx context.Context, req api.GetInternalSessionRequestObject) (api.GetInternalSessionResponseObject, error) {
	s, err := h.svc.InternalSession(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.GetInternalSession200JSONResponse(adminSession(s, nil)), nil
}

// customerDetail is what a customer may see of a failure.
var customerDetail = []string{"shortages", "decline_reason"}

func session(s domain.Session, admin bool) api.Session {
	m := func(minor int64) money.Money { return money.Money{AmountMinor: minor, Currency: s.Currency} }
	items := make([]api.SessionItem, len(s.Items))
	for i, it := range s.Items {
		items[i] = api.SessionItem{
			SkuID: parseUUID(it.SKUID), ProductID: uuidPtr(it.ProductID), SkuCode: it.SKUCode, Name: it.Name,
			Quantity: it.Quantity, IsDigital: it.IsDigital, InventoryTracked: it.InventoryTracked,
			UnitPrice: m(it.UnitPriceMinor), LineTotal: m(it.LineTotalMinor), Tax: m(it.TaxMinor),
		}
		if len(it.VariantValues) > 0 {
			v := it.VariantValues
			items[i].VariantValues = &v
		}
	}
	options := make([]api.ShippingOption, len(s.ShippingOptions))
	for i, o := range s.ShippingOptions {
		options[i] = api.ShippingOption{
			ID: o.ID, Name: o.Name, Amount: m(o.AmountMinor),
			EstimatedDaysMin: o.EstimatedDaysMin, EstimatedDaysMax: o.EstimatedDaysMax,
		}
	}
	out := api.Session{
		SessionID: parseUUID(s.ID), State: api.State(s.State), UserType: api.SessionUserType(s.UserType),
		CartType: s.CartType, Items: items, RequiresShipping: s.RequiresShipping, ShippingOptions: options,
		SelectedShippingOptionID: optional(s.ShippingOptionID), TaxInclusive: s.TaxInclusive,
		Totals: api.Totals{
			ItemsTotal: m(s.ItemsTotalMinor), TaxTotal: m(s.TaxTotalMinor), Shipping: m(s.ShippingMinor), Total: m(s.TotalMinor),
		},
		ExpiresAt: s.ExpiresAt.UTC(), ConfirmedAt: utc(s.ConfirmedAt), CompletedAt: utc(s.CompletedAt),
		CreatedAt: s.CreatedAt.UTC(), UpdatedAt: s.UpdatedAt.UTC(),
	}
	if s.PaymentGateway != "" {
		p := &api.PaymentState{
			Gateway: s.PaymentGateway, Flow: api.PaymentStateFlow(s.Variant()), Hosted: s.HostedPayment,
			PublicKey: optional(s.PaymentPublicKey), Status: optional(s.PaymentStatus),
			TransactionID: uuidPtr(s.PaymentTransactionID),
		}
		if len(s.NextAction) > 0 {
			na := s.NextAction
			p.NextAction = &na
		}
		out.Payment = p
	}
	if s.ReservationID != "" {
		out.Reservation = &api.ReservationState{ReservationID: parseUUID(s.ReservationID), ExpiresAt: utc(s.ReservationExpiresAt)}
	}
	if s.OrderID != "" {
		out.Order = &api.OrderRef{OrderID: parseUUID(s.OrderID), OrderNumber: optional(s.OrderNumber)}
	}
	if s.FailureReason != "" {
		f := &api.Failure{State: api.State(s.FailedState), Reason: s.FailureReason}
		detail := s.FailureDetail
		if !admin {
			detail = map[string]any{}
			for _, k := range customerDetail {
				if v, ok := s.FailureDetail[k]; ok && v != nil && v != "" {
					detail[k] = v
				}
			}
		}
		if len(detail) > 0 {
			f.Detail = &detail
		}
		out.Failure = f
	}
	return out
}

func adminSession(s domain.Session, history []repository.Event) api.AdminSession {
	b := session(s, true)
	out := api.AdminSession{
		SessionID: b.SessionID, State: b.State, UserType: api.AdminSessionUserType(b.UserType), CartType: b.CartType,
		Items: b.Items, RequiresShipping: b.RequiresShipping, ShippingOptions: b.ShippingOptions,
		SelectedShippingOptionID: b.SelectedShippingOptionID, TaxInclusive: b.TaxInclusive, Totals: b.Totals,
		Payment: b.Payment, Reservation: b.Reservation, Order: b.Order, Failure: b.Failure,
		ExpiresAt: b.ExpiresAt, ConfirmedAt: b.ConfirmedAt, CompletedAt: b.CompletedAt,
		CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt,
		UserID: s.UserID, Attempts: s.Attempts, NextAttemptAt: utc(s.NextAttemptAt),
		CompensationLog: make([]api.CompensationEntry, len(s.CompensationLog)),
	}
	for i, e := range s.CompensationLog {
		out.CompensationLog[i] = api.CompensationEntry{Step: api.CompensationEntryStep(e.Step), Result: e.Result, At: e.At.UTC()}
	}
	if g := s.GuestContact; g != nil {
		out.GuestContact = &api.GuestContact{Email: g.Email, PhoneE164: g.PhoneE164}
	}
	if len(s.DeliveryAddress) > 0 {
		a := s.DeliveryAddress
		out.DeliveryAddress = &a
	}
	if history != nil {
		h := make([]api.HistoryEntry, len(history))
		for i, e := range history {
			h[i] = api.HistoryEntry{EventType: e.EventType, CreatedAt: e.CreatedAt.UTC()}
			if e.From != "" {
				st := api.State(e.From)
				h[i].FromState = &st
			}
			if e.To != "" {
				st := api.State(e.To)
				h[i].ToState = &st
			}
			if len(e.Payload) > 0 {
				p := e.Payload
				h[i].Payload = &p
			}
		}
		out.History = &h
	}
	return out
}

func parseUUID(s string) uuid.UUID {
	id, _ := uuid.Parse(s)
	return id
}

func uuidPtr(s string) *uuid.UUID {
	if s == "" {
		return nil
	}
	id := parseUUID(s)
	return &id
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
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
