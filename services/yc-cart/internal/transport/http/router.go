// Package http is yc-cart's HTTP transport. Routing, parameter parsing and
// request/response types are generated from api/openapi.yaml.
package http

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	money "github.com/slaghuis/yc-platform/libs/yc-money"
	api "github.com/slaghuis/yc-platform/services/yc-cart/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-cart/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-cart/internal/repository"
	"github.com/slaghuis/yc-platform/services/yc-cart/internal/service"
)

// Carts is the service the handlers call.
type Carts interface {
	List(ctx context.Context) ([]service.Summary, error)
	Get(ctx context.Context, cartType, displayCurrency string) (domain.View, error)
	AddItem(ctx context.Context, cartType, skuID string, quantity int, displayCurrency string) (domain.View, error)
	SetQuantity(ctx context.Context, cartType, skuID string, quantity int, displayCurrency string) (domain.View, error)
	RemoveItem(ctx context.Context, cartType, skuID string) error
	MoveItem(ctx context.Context, from, skuID, to string, quantity int, displayCurrency string) (domain.View, error)
	Clear(ctx context.Context, cartType string) error
	InternalCart(ctx context.Context, userID, cartType string) (domain.Cart, domain.CartType, error)
	AbandonedCarts(ctx context.Context, after *repository.AbandonedCursor, limit int) ([]repository.Abandoned, *repository.AbandonedCursor, error)
}

// NewRouter returns the service's routes on the standard httpkit chain.
func NewRouter(svc Carts, build httpkit.BuildInfo, checks []httpkit.Check) http.Handler {
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

// Routes are the gateway routes yc-cart registers (ADR-020 rule 7).
func Routes() []httpkit.RegisteredRoute {
	return []httpkit.RegisteredRoute{
		{Prefix: "/carts", Roles: []string{string(authctx.ActorVisitor), string(authctx.ActorCustomer)}},
		{Prefix: "/internal/carts", Methods: []string{http.MethodGet}},
		{Prefix: "/admin/abandoned-carts", Methods: []string{http.MethodGet}, Roles: []string{string(authctx.ActorShopAdmin)}},
	}
}

func (h handlers) ListAbandonedCarts(ctx context.Context, req api.ListAbandonedCartsRequestObject) (api.ListAbandonedCartsResponseObject, error) {
	page, err := httpkit.ParsePage(req.Params.PageSize, req.Params.Cursor)
	if err != nil {
		return nil, err
	}
	var after *repository.AbandonedCursor
	if page.Cursor != "" {
		after = &repository.AbandonedCursor{}
		if err := httpkit.DecodeCursor(page.Cursor, after); err != nil {
			return nil, err
		}
	}
	rows, next, err := h.svc.AbandonedCarts(ctx, after, page.Size)
	if err != nil {
		return nil, err
	}
	info := httpkit.PageInfo{Size: page.Size, Cursor: page.Cursor}
	if next != nil {
		info.NextCursor = httpkit.EncodeCursor(next)
	}
	out := api.ListAbandonedCarts200JSONResponse{Items: make([]api.AbandonedCart, len(rows)), Page: info}
	for i, c := range rows {
		out.Items[i] = api.AbandonedCart{
			CartID: uuid.MustParse(c.CartID), UserID: uuid.MustParse(c.UserID), CartType: c.CartType,
			ItemCount: c.Lines, Quantity: c.Quantity, LastActivityAt: c.LastActivityAt.UTC(), AbandonedAt: c.AbandonedAt.UTC(),
		}
	}
	return out, nil
}

func (h handlers) GetInternalCart(ctx context.Context, req api.GetInternalCartRequestObject) (api.GetInternalCartResponseObject, error) {
	cart, t, err := h.svc.InternalCart(ctx, req.UserID.String(), req.CartType)
	if err != nil {
		return nil, err
	}
	out := api.GetInternalCart200JSONResponse{
		CartID: cartID(cart.ID), UserID: req.UserID, CartType: t.Name, Convertible: t.Convertible,
		Items: make([]api.InternalCartItem, len(cart.Items)),
	}
	for i, it := range cart.Items {
		sku, _ := uuid.Parse(it.SKUID)
		out.Items[i] = api.InternalCartItem{SkuID: sku, Quantity: it.Quantity, UnitPrice: it.UnitPrice, AddedAt: it.AddedAt.UTC()}
	}
	return out, nil
}

type handlers struct {
	svc Carts
}

var _ api.StrictServerInterface = handlers{}

func (h handlers) ListCarts(ctx context.Context, _ api.ListCartsRequestObject) (api.ListCartsResponseObject, error) {
	carts, err := h.svc.List(ctx)
	if err != nil {
		return nil, err
	}
	out := api.ListCarts200JSONResponse{Items: make([]api.CartSummary, len(carts))}
	for i, c := range carts {
		out.Items[i] = api.CartSummary{
			CartID: cartID(c.CartID), CartType: c.Type.Name, Convertible: c.Type.Convertible,
			LineCount: c.Lines, Quantity: c.Quantity,
		}
	}
	return out, nil
}

func (h handlers) GetCart(ctx context.Context, req api.GetCartRequestObject) (api.GetCartResponseObject, error) {
	v, err := h.svc.Get(ctx, req.CartType, displayCurrency(req.Params.DisplayCurrency, req.Params.XYCDisplayCurrency))
	if err != nil {
		return nil, err
	}
	return api.GetCart200JSONResponse(cart(v)), nil
}

func (h handlers) ClearCart(ctx context.Context, req api.ClearCartRequestObject) (api.ClearCartResponseObject, error) {
	if err := h.svc.Clear(ctx, req.CartType); err != nil {
		return nil, err
	}
	return api.ClearCart204Response{}, nil
}

func (h handlers) AddItem(ctx context.Context, req api.AddItemRequestObject) (api.AddItemResponseObject, error) {
	quantity := 1
	if req.Body.Quantity != nil {
		quantity = *req.Body.Quantity
	}
	v, err := h.svc.AddItem(ctx, req.CartType, req.Body.SkuID.String(), quantity,
		displayCurrency(req.Params.DisplayCurrency, req.Params.XYCDisplayCurrency))
	if err != nil {
		return nil, err
	}
	return api.AddItem200JSONResponse(cart(v)), nil
}

func (h handlers) SetItemQuantity(ctx context.Context, req api.SetItemQuantityRequestObject) (api.SetItemQuantityResponseObject, error) {
	v, err := h.svc.SetQuantity(ctx, req.CartType, req.SkuID.String(), req.Body.Quantity,
		displayCurrency(req.Params.DisplayCurrency, req.Params.XYCDisplayCurrency))
	if err != nil {
		return nil, err
	}
	return api.SetItemQuantity200JSONResponse(cart(v)), nil
}

func (h handlers) RemoveItem(ctx context.Context, req api.RemoveItemRequestObject) (api.RemoveItemResponseObject, error) {
	if err := h.svc.RemoveItem(ctx, req.CartType, req.SkuID.String()); err != nil {
		return nil, err
	}
	return api.RemoveItem204Response{}, nil
}

func (h handlers) MoveItem(ctx context.Context, req api.MoveItemRequestObject) (api.MoveItemResponseObject, error) {
	quantity := 0
	if req.Body.Quantity != nil {
		quantity = *req.Body.Quantity
	}
	v, err := h.svc.MoveItem(ctx, req.CartType, req.SkuID.String(), req.Body.ToCartType, quantity,
		displayCurrency(req.Params.DisplayCurrency, req.Params.XYCDisplayCurrency))
	if err != nil {
		return nil, err
	}
	return api.MoveItem200JSONResponse(cart(v)), nil
}

func displayCurrency(query, header *string) string {
	if query != nil && *query != "" {
		return *query
	}
	if header != nil {
		return *header
	}
	return ""
}

func cart(v domain.View) api.Cart {
	out := api.Cart{
		CartID: cartID(v.Cart.ID), CartType: v.Type.Name, Convertible: v.Type.Convertible,
		Items: make([]api.CartItem, len(v.Lines)), LineCount: len(v.Lines), Quantity: v.Cart.Quantity(),
		PriceChanged: v.PriceChange,
	}
	if v.Currency != "" {
		out.Currency, out.Subtotal = &v.Currency, &v.Subtotal
	}
	if !v.Cart.UpdatedAt.IsZero() {
		t := v.Cart.UpdatedAt.UTC()
		out.UpdatedAt = &t
	}
	if d := v.Display; d != nil {
		out.Display = &api.CartDisplay{
			Currency: d.Currency, Rate: d.Rate, RateFetchedAt: d.RateFetchedAt.UTC(), Subtotal: d.Subtotal,
		}
	}
	for i, l := range v.Lines {
		out.Items[i] = item(l)
	}
	return out
}

func item(l domain.LineView) api.CartItem {
	id, _ := uuid.Parse(l.SKUID)
	out := api.CartItem{
		SkuID: id, Quantity: l.Quantity, UnitPrice: l.UnitPrice, Available: l.Available,
		PriceChanged: l.PriceChanged, AddedAt: l.AddedAt.UTC(),
	}
	if !l.Available {
		out.UnavailableReason = &l.Reason
		return out
	}
	out.CurrentUnitPrice, out.LineTotal = ptr(l.Current), ptr(l.Total)
	if d := l.Display; d != nil {
		out.Display = &struct {
			LineTotal money.Money `json:"line_total"`
			UnitPrice money.Money `json:"unit_price"`
		}{LineTotal: d.Total, UnitPrice: d.UnitPrice}
	}
	return out
}

func cartID(id string) *uuid.UUID {
	if id == "" {
		return nil
	}
	u, err := uuid.Parse(id)
	if err != nil {
		return nil
	}
	return &u
}

func ptr[T any](v T) *T { return &v }
