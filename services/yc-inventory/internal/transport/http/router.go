// Package http is yc-inventory's HTTP transport. Routing, parameter
// parsing and request/response types are generated from api/openapi.yaml.
package http

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	api "github.com/slaghuis/yc-platform/services/yc-inventory/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-inventory/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-inventory/internal/repository"
	"github.com/slaghuis/yc-platform/services/yc-inventory/internal/service"
)

// Inventory is the service the handlers call.
type Inventory interface {
	Config(ctx context.Context) (domain.Config, error)
	PutConfig(ctx context.Context, c domain.Config) (domain.Config, error)
	ListStock(ctx context.Context, f repository.LevelFilter, after string, limit int) ([]domain.Level, string, error)
	Stock(ctx context.Context, skuID string) (domain.Level, error)
	Receive(ctx context.Context, skuID string, quantity int64, reason string) (domain.Level, error)
	Adjust(ctx context.Context, skuID string, delta int64, reason string) (domain.Level, error)
	SetThreshold(ctx context.Context, skuID string, threshold int64) (domain.Level, error)
	SKUReservations(ctx context.Context, skuID string) ([]domain.Reservation, error)
	History(ctx context.Context, skuID string, after *repository.MovementCursor, limit int) ([]domain.Movement, *repository.MovementCursor, error)
	AdminRelease(ctx context.Context, id string) (domain.Reservation, error)
	Reserve(ctx context.Context, req service.ReserveRequest) (*domain.Reservation, error)
	Commit(ctx context.Context, id, reference string) (service.CommitResult, error)
	Release(ctx context.Context, id string) (domain.Reservation, bool, error)
}

// NewRouter returns the service's routes on the standard httpkit chain.
// POSTs carrying an Idempotency-Key are replayed from idem.
func NewRouter(svc Inventory, idem httpkit.IdempotencyStore, build httpkit.BuildInfo, checks []httpkit.Check) http.Handler {
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

// Routes are the gateway routes yc-inventory registers (ADR-020 rule 7).
func Routes() []httpkit.RegisteredRoute {
	admin := []string{string(authctx.ActorShopAdmin)}
	return []httpkit.RegisteredRoute{
		{Prefix: "/admin/inventory", Roles: admin},
		{Prefix: "/admin/stock", Roles: admin},
		{Prefix: "/admin/reservations", Roles: admin},
		{Prefix: "/internal/reservations", Methods: []string{http.MethodPost}},
	}
}

type handlers struct {
	svc Inventory
}

var _ api.StrictServerInterface = handlers{}

func (h handlers) GetInventoryConfig(ctx context.Context, _ api.GetInventoryConfigRequestObject) (api.GetInventoryConfigResponseObject, error) {
	c, err := h.svc.Config(ctx)
	if err != nil {
		return nil, err
	}
	return api.GetInventoryConfig200JSONResponse(config(c)), nil
}

func (h handlers) PutInventoryConfig(ctx context.Context, req api.PutInventoryConfigRequestObject) (api.PutInventoryConfigResponseObject, error) {
	b := req.Body
	c, err := h.svc.PutConfig(ctx, domain.Config{
		Enabled: b.InventoryEnabled, DefaultTTL: b.DefaultReservationTTLMinutes,
		MinTTL: b.MinReservationTTLMinutes, MaxTTL: b.MaxReservationTTLMinutes,
	})
	if err != nil {
		return nil, err
	}
	return api.PutInventoryConfig200JSONResponse(config(c)), nil
}

func (h handlers) ListStock(ctx context.Context, req api.ListStockRequestObject) (api.ListStockResponseObject, error) {
	page, err := httpkit.ParsePage(req.Params.PageSize, req.Params.Cursor)
	if err != nil {
		return nil, err
	}
	var after string
	if page.Cursor != "" {
		if err := httpkit.DecodeCursor(page.Cursor, &after); err != nil {
			return nil, err
		}
	}
	f := repository.LevelFilter{BelowThreshold: deref(req.Params.BelowThreshold), OutOfStock: deref(req.Params.OutOfStock)}
	levels, next, err := h.svc.ListStock(ctx, f, after, page.Size)
	if err != nil {
		return nil, err
	}
	info := httpkit.PageInfo{Size: page.Size, Cursor: page.Cursor}
	if next != "" {
		info.NextCursor = httpkit.EncodeCursor(next)
	}
	out := api.ListStock200JSONResponse{Items: make([]api.StockLevel, len(levels)), Page: info}
	for i, l := range levels {
		out.Items[i] = stockLevel(l)
	}
	return out, nil
}

func (h handlers) GetStock(ctx context.Context, req api.GetStockRequestObject) (api.GetStockResponseObject, error) {
	l, err := h.svc.Stock(ctx, req.SkuID.String())
	if err != nil {
		return nil, err
	}
	return api.GetStock200JSONResponse(stockLevel(l)), nil
}

func (h handlers) ReceiveStock(ctx context.Context, req api.ReceiveStockRequestObject) (api.ReceiveStockResponseObject, error) {
	l, err := h.svc.Receive(ctx, req.SkuID.String(), req.Body.Quantity, deref(req.Body.Reason))
	if err != nil {
		return nil, err
	}
	return api.ReceiveStock200JSONResponse(stockLevel(l)), nil
}

func (h handlers) AdjustStock(ctx context.Context, req api.AdjustStockRequestObject) (api.AdjustStockResponseObject, error) {
	l, err := h.svc.Adjust(ctx, req.SkuID.String(), req.Body.Delta, req.Body.Reason)
	if err != nil {
		return nil, err
	}
	return api.AdjustStock200JSONResponse(stockLevel(l)), nil
}

func (h handlers) SetLowThreshold(ctx context.Context, req api.SetLowThresholdRequestObject) (api.SetLowThresholdResponseObject, error) {
	l, err := h.svc.SetThreshold(ctx, req.SkuID.String(), req.Body.LowThreshold)
	if err != nil {
		return nil, err
	}
	return api.SetLowThreshold200JSONResponse(stockLevel(l)), nil
}

func (h handlers) ListSKUReservations(ctx context.Context, req api.ListSKUReservationsRequestObject) (api.ListSKUReservationsResponseObject, error) {
	rs, err := h.svc.SKUReservations(ctx, req.SkuID.String())
	if err != nil {
		return nil, err
	}
	out := api.ListSKUReservations200JSONResponse{Items: make([]api.Reservation, len(rs))}
	for i, r := range rs {
		out.Items[i] = reservation(r)
	}
	return out, nil
}

func (h handlers) ListStockHistory(ctx context.Context, req api.ListStockHistoryRequestObject) (api.ListStockHistoryResponseObject, error) {
	page, err := httpkit.ParsePage(req.Params.PageSize, req.Params.Cursor)
	if err != nil {
		return nil, err
	}
	var after *repository.MovementCursor
	if page.Cursor != "" {
		after = &repository.MovementCursor{}
		if err := httpkit.DecodeCursor(page.Cursor, after); err != nil {
			return nil, err
		}
	}
	ms, next, err := h.svc.History(ctx, req.SkuID.String(), after, page.Size)
	if err != nil {
		return nil, err
	}
	info := httpkit.PageInfo{Size: page.Size, Cursor: page.Cursor}
	if next != nil {
		info.NextCursor = httpkit.EncodeCursor(next)
	}
	out := api.ListStockHistory200JSONResponse{Items: make([]api.Movement, len(ms)), Page: info}
	for i, m := range ms {
		id, _ := uuid.Parse(m.ID)
		out.Items[i] = api.Movement{
			MovementID: id, Type: api.MovementType(m.Type), OnHandDelta: m.OnHandDelta, ReservedDelta: m.ReservedDelta,
			Reason: optional(m.Reason), ActorID: optional(m.ActorID), Reference: optional(m.Reference), CreatedAt: m.CreatedAt.UTC(),
		}
	}
	return out, nil
}

func (h handlers) AdminReleaseReservation(ctx context.Context, req api.AdminReleaseReservationRequestObject) (api.AdminReleaseReservationResponseObject, error) {
	r, err := h.svc.AdminRelease(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.AdminReleaseReservation200JSONResponse(reservation(r)), nil
}

func (h handlers) ReserveStock(ctx context.Context, req api.ReserveStockRequestObject) (api.ReserveStockResponseObject, error) {
	items := make([]domain.Line, len(req.Body.Items))
	for i, l := range req.Body.Items {
		items[i] = domain.Line{SKUID: l.SkuID.String(), Quantity: l.Quantity}
	}
	r, err := h.svc.Reserve(ctx, service.ReserveRequest{
		SessionID: req.Body.SessionID.String(), IdempotencyKey: req.Params.IdempotencyKey,
		Items: items, TTLMinutes: req.Body.TTLMinutes,
	})
	if err != nil {
		return nil, err
	}
	out := api.ReserveStock200JSONResponse{Items: []api.Line{}}
	if r != nil {
		id, _ := uuid.Parse(r.ID)
		status := api.ReserveResultStatus(r.Status)
		expires := r.ExpiresAt.UTC()
		out.ReservationID, out.Status, out.ExpiresAt, out.Items = &id, &status, &expires, lines(r.Items)
	}
	return out, nil
}

func (h handlers) CommitReservation(ctx context.Context, req api.CommitReservationRequestObject) (api.CommitReservationResponseObject, error) {
	var reference string
	if req.Body != nil {
		reference = deref(req.Body.Reference)
	}
	res, err := h.svc.Commit(ctx, req.ID.String(), reference)
	if err != nil {
		return nil, err
	}
	out := api.CommitReservation200JSONResponse{
		ReservationID: req.ID, Status: api.CommitResultStatus(domain.ReservationCommitted), Items: lines(res.Reservation.Items),
	}
	if len(res.Oversold) > 0 {
		o := lines(res.Oversold)
		out.Oversold = &o
	}
	return out, nil
}

func (h handlers) ReleaseReservation(ctx context.Context, req api.ReleaseReservationRequestObject) (api.ReleaseReservationResponseObject, error) {
	r, found, err := h.svc.Release(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	status := api.ReleaseResultStatus("unknown")
	if found {
		status = api.ReleaseResultStatus(r.Status)
	}
	return api.ReleaseReservation200JSONResponse{ReservationID: req.ID, Status: status}, nil
}

func config(c domain.Config) api.InventoryConfig {
	out := api.InventoryConfig{
		InventoryEnabled: c.Enabled, DefaultReservationTTLMinutes: c.DefaultTTL,
		MinReservationTTLMinutes: c.MinTTL, MaxReservationTTLMinutes: c.MaxTTL,
	}
	if c.Stored {
		at := c.UpdatedAt.UTC()
		out.UpdatedAt = &at
	}
	return out
}

func stockLevel(l domain.Level) api.StockLevel {
	id, _ := uuid.Parse(l.SKUID)
	return api.StockLevel{
		SkuID: id, OnHand: l.OnHand, Reserved: l.Reserved, Available: l.Available(), LowThreshold: l.LowThreshold,
		UpdatedAt: l.UpdatedAt.UTC(),
	}
}

func reservation(r domain.Reservation) api.Reservation {
	id, _ := uuid.Parse(r.ID)
	session, _ := uuid.Parse(r.SessionID)
	out := api.Reservation{
		ReservationID: id, SessionID: session, Status: api.ReservationStatus(r.Status),
		ExpiresAt: r.ExpiresAt.UTC(), CreatedAt: r.CreatedAt.UTC(), Items: lines(r.Items),
	}
	if r.SettledAt != nil {
		at := r.SettledAt.UTC()
		out.SettledAt = &at
	}
	return out
}

func lines(ls []domain.Line) []api.Line {
	out := make([]api.Line, len(ls))
	for i, l := range ls {
		id, _ := uuid.Parse(l.SKUID)
		out[i] = api.Line{SkuID: id, Quantity: l.Quantity}
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
