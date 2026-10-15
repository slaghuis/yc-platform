// Package http is yc-orders' HTTP transport. Routing, parameter parsing
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

	api "github.com/slaghuis/yc-platform/services/yc-orders/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-orders/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-orders/internal/repository"
)

// Orders is the service the handlers call.
type Orders interface {
	Create(ctx context.Context, o domain.Order) (domain.Order, error)
	Confirm(ctx context.Context, id, paymentTransactionID string) (domain.Order, error)
	Abandon(ctx context.Context, id, reason string) (domain.Order, bool, error)
	Cancel(ctx context.Context, id, reason string) (domain.Order, error)
	Order(ctx context.Context, id string) (domain.Order, []domain.HistoryEntry, error)
	AdminOrder(ctx context.Context, id string) (domain.Order, []domain.HistoryEntry, error)
	CustomerOrder(ctx context.Context, id string) (domain.Order, []domain.HistoryEntry, error)
	Mine(ctx context.Context, after *repository.OrderCursor, limit int) ([]domain.Order, error)
	AdminList(ctx context.Context, status domain.Status, after *repository.OrderCursor, limit int) ([]domain.Order, error)
}

// NewRouter returns the service's routes on the standard httpkit chain.
// POSTs carrying an Idempotency-Key are replayed from idem.
func NewRouter(svc Orders, idem httpkit.IdempotencyStore, build httpkit.BuildInfo, checks []httpkit.Check) http.Handler {
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

// Routes are the gateway routes yc-orders registers (ADR-020 rule 7).
func Routes() []httpkit.RegisteredRoute {
	admin := []string{string(authctx.ActorShopAdmin)}
	return []httpkit.RegisteredRoute{
		{
			Prefix: "/orders", Methods: []string{http.MethodGet},
			Roles: []string{string(authctx.ActorCustomer), string(authctx.ActorVisitor)},
		},
		{Prefix: "/admin/orders", Roles: admin},
		{Prefix: "/internal/orders"},
	}
}

type handlers struct {
	svc Orders
}

var _ api.StrictServerInterface = handlers{}

func (h handlers) ListMyOrders(ctx context.Context, req api.ListMyOrdersRequestObject) (api.ListMyOrdersResponseObject, error) {
	page, after, err := parsePage(req.Params.PageSize, req.Params.Cursor)
	if err != nil {
		return nil, err
	}
	os, err := h.svc.Mine(ctx, after, page.Size+1)
	if err != nil {
		return nil, err
	}
	items, info := summaries(os, page)
	return api.ListMyOrders200JSONResponse{Items: items, Page: info}, nil
}

func (h handlers) GetMyOrder(ctx context.Context, req api.GetMyOrderRequestObject) (api.GetMyOrderResponseObject, error) {
	o, history, err := h.svc.CustomerOrder(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.GetMyOrder200JSONResponse(order(o, history)), nil
}

func (h handlers) ListOrders(ctx context.Context, req api.ListOrdersRequestObject) (api.ListOrdersResponseObject, error) {
	page, after, err := parsePage(req.Params.PageSize, req.Params.Cursor)
	if err != nil {
		return nil, err
	}
	var status domain.Status
	if req.Params.Status != nil {
		status = domain.Status(*req.Params.Status)
	}
	os, err := h.svc.AdminList(ctx, status, after, page.Size+1)
	if err != nil {
		return nil, err
	}
	items, info := summaries(os, page)
	return api.ListOrders200JSONResponse{Items: items, Page: info}, nil
}

func (h handlers) GetOrder(ctx context.Context, req api.GetOrderRequestObject) (api.GetOrderResponseObject, error) {
	o, history, err := h.svc.AdminOrder(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.GetOrder200JSONResponse(order(o, history)), nil
}

func (h handlers) CancelOrder(ctx context.Context, req api.CancelOrderRequestObject) (api.CancelOrderResponseObject, error) {
	var reason string
	if req.Body != nil {
		reason = deref(req.Body.Reason)
	}
	o, err := h.svc.Cancel(ctx, req.ID.String(), reason)
	if err != nil {
		return nil, err
	}
	return api.CancelOrder200JSONResponse(order(o, nil)), nil
}

func (h handlers) CreateOrder(ctx context.Context, req api.CreateOrderRequestObject) (api.CreateOrderResponseObject, error) {
	b := req.Body
	pricing, err := json.Marshal(b.PricingSnapshot)
	if err != nil {
		return nil, err
	}
	o := domain.Order{
		CheckoutSessionID: b.CheckoutSessionID.String(), UserID: b.UserID, UserType: domain.UserType(b.UserType),
		RequiresShipping: b.RequiresShipping, Currency: b.Currency, ItemsTotalMinor: b.ItemsTotalMinor,
		ShippingMinor: b.ShippingMinor, TaxTotalMinor: b.TaxTotalMinor, TotalMinor: b.TotalMinor,
		PricingSnapshot: pricing, Items: make([]domain.Item, len(b.Items)),
	}
	if b.GuestContact != nil {
		o.GuestContact = &domain.GuestContact{Email: b.GuestContact.Email, PhoneE164: b.GuestContact.PhoneE164}
	}
	if b.DeliveryAddress != nil {
		o.DeliveryAddress = *b.DeliveryAddress
	}
	if b.ShippingOption != nil {
		if o.ShippingSnapshot, err = json.Marshal(*b.ShippingOption); err != nil {
			return nil, err
		}
	}
	if b.Payment != nil {
		o.PaymentGateway = b.Payment.Gateway
		if b.Payment.TransactionID != nil {
			o.PaymentTransactionID = b.Payment.TransactionID.String()
		}
	}
	for i, it := range b.Items {
		item := domain.Item{
			SKUID: it.SkuID.String(), SKUCode: deref(it.SkuCode), NameSnapshot: it.NameSnapshot, Quantity: it.Quantity,
			UnitPriceMinor: it.UnitPriceMinor, LineTotalMinor: it.LineTotalMinor, TaxMinor: deref(it.TaxMinor),
			IsDigital: deref(it.IsDigital),
		}
		if it.ProductID != nil {
			item.ProductID = it.ProductID.String()
		}
		if it.VariantValuesSnapshot != nil {
			item.VariantValues = *it.VariantValuesSnapshot
		}
		o.Items[i] = item
	}
	created, err := h.svc.Create(ctx, o)
	if err != nil {
		return nil, err
	}
	return api.CreateOrder201JSONResponse(order(created, nil)), nil
}

func (h handlers) GetInternalOrder(ctx context.Context, req api.GetInternalOrderRequestObject) (api.GetInternalOrderResponseObject, error) {
	o, history, err := h.svc.Order(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.GetInternalOrder200JSONResponse(order(o, history)), nil
}

func (h handlers) ConfirmOrder(ctx context.Context, req api.ConfirmOrderRequestObject) (api.ConfirmOrderResponseObject, error) {
	var tx string
	if req.Body != nil && req.Body.PaymentTransactionID != nil {
		tx = req.Body.PaymentTransactionID.String()
	}
	o, err := h.svc.Confirm(ctx, req.ID.String(), tx)
	if err != nil {
		return nil, err
	}
	return api.ConfirmOrder200JSONResponse(order(o, nil)), nil
}

func (h handlers) AbandonOrder(ctx context.Context, req api.AbandonOrderRequestObject) (api.AbandonOrderResponseObject, error) {
	var reason string
	if req.Body != nil {
		reason = deref(req.Body.Reason)
	}
	_, found, err := h.svc.Abandon(ctx, req.ID.String(), reason)
	if err != nil {
		return nil, err
	}
	if !found {
		return api.AbandonOrder200JSONResponse{Status: "none"}, nil
	}
	id := req.ID
	return api.AbandonOrder200JSONResponse{Status: "abandoned", OrderID: &id}, nil
}

func parsePage(size *int, cursor *string) (httpkit.PageRequest, *repository.OrderCursor, error) {
	page, err := httpkit.ParsePage(size, cursor)
	if err != nil {
		return page, nil, err
	}
	if page.Cursor == "" {
		return page, nil, nil
	}
	after := &repository.OrderCursor{}
	return page, after, httpkit.DecodeCursor(page.Cursor, after)
}

// summaries trims the extra row fetched to detect a next page.
func summaries(os []domain.Order, page httpkit.PageRequest) ([]api.OrderSummary, httpkit.PageInfo) {
	info := httpkit.PageInfo{Size: page.Size, Cursor: page.Cursor}
	if len(os) > page.Size {
		os = os[:page.Size]
		last := os[len(os)-1]
		info.NextCursor = httpkit.EncodeCursor(repository.OrderCursor{At: last.CreatedAt, ID: last.ID})
	}
	out := make([]api.OrderSummary, len(os))
	for i, o := range os {
		id, _ := uuid.Parse(o.ID)
		var n int64
		for _, it := range o.Items {
			n += it.Quantity
		}
		out[i] = api.OrderSummary{
			OrderID: id, OrderNumber: optional(o.OrderNumber), Status: api.OrderStatus(o.Status),
			UserType: api.OrderSummaryUserType(o.UserType), RequiresShipping: o.RequiresShipping,
			Total: money(o.TotalMinor, o.Currency), ItemCount: n, CreatedAt: o.CreatedAt.UTC(), PlacedAt: utc(o.PlacedAt),
		}
	}
	return out, info
}

func order(o domain.Order, history []domain.HistoryEntry) api.Order {
	id, _ := uuid.Parse(o.ID)
	session, _ := uuid.Parse(o.CheckoutSessionID)
	out := api.Order{
		OrderID: id, OrderNumber: optional(o.OrderNumber), CheckoutSessionID: session, UserID: o.UserID,
		UserType: api.OrderUserType(o.UserType), RequiresShipping: o.RequiresShipping, Status: api.OrderStatus(o.Status),
		Items: make([]api.OrderItem, len(o.Items)), ItemsTotal: money(o.ItemsTotalMinor, o.Currency),
		Shipping: money(o.ShippingMinor, o.Currency), TaxTotal: money(o.TaxTotalMinor, o.Currency),
		Total: money(o.TotalMinor, o.Currency), Refunded: money(o.RefundedMinor, o.Currency),
		CreatedAt: o.CreatedAt.UTC(), UpdatedAt: o.UpdatedAt.UTC(), PlacedAt: utc(o.PlacedAt),
	}
	if g := o.GuestContact; g != nil {
		out.GuestContact = &api.GuestContact{Email: g.Email, PhoneE164: g.PhoneE164}
	}
	if o.DeliveryAddress != nil {
		addr := o.DeliveryAddress
		out.DeliveryAddress = &addr
	}
	if o.PaymentGateway != "" || o.PaymentTransactionID != "" {
		p := api.Payment{Gateway: o.PaymentGateway}
		if tx, err := uuid.Parse(o.PaymentTransactionID); err == nil {
			p.TransactionID = &tx
		}
		out.Payment = &p
	}
	for i, it := range o.Items {
		sku, _ := uuid.Parse(it.SKUID)
		item := api.OrderItem{
			SkuID: sku, SkuCode: it.SKUCode, NameSnapshot: it.NameSnapshot, Quantity: it.Quantity,
			UnitPrice: money(it.UnitPriceMinor, o.Currency), LineTotal: money(it.LineTotalMinor, o.Currency),
			Tax: money(it.TaxMinor, o.Currency), IsDigital: it.IsDigital,
		}
		if p, err := uuid.Parse(it.ProductID); err == nil {
			item.ProductID = &p
		}
		if it.VariantValues != nil {
			v := it.VariantValues
			item.VariantValuesSnapshot = &v
		}
		out.Items[i] = item
	}
	if history != nil {
		hs := make([]api.HistoryEntry, len(history))
		for i, h := range history {
			hs[i] = api.HistoryEntry{ToStatus: api.OrderStatus(h.To), Reason: optional(h.Reason), CreatedAt: h.CreatedAt.UTC()}
			if h.From != "" {
				from := api.OrderStatus(h.From)
				hs[i].FromStatus = &from
			}
		}
		out.History = &hs
	}
	return out
}

func money(minor int64, currency string) api.Money {
	return api.Money{AmountMinor: minor, Currency: currency}
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
