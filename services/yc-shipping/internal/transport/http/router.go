// Package http is yc-shipping's HTTP transport. Routing, parameter parsing
// and request/response types are generated from api/openapi.yaml.
package http

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	api "github.com/slaghuis/yc-platform/services/yc-shipping/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-shipping/internal/carrier"
	"github.com/slaghuis/yc-platform/services/yc-shipping/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-shipping/internal/repository"
	"github.com/slaghuis/yc-platform/services/yc-shipping/internal/service"
)

// Shipping is the service the handlers call.
type Shipping interface {
	Zones(ctx context.Context) ([]domain.Zone, error)
	PublicZones(ctx context.Context) ([]domain.Zone, error)
	CreateZone(ctx context.Context, z domain.Zone) (domain.Zone, error)
	UpdateZone(ctx context.Context, id string, z domain.Zone) (domain.Zone, error)
	DeleteZone(ctx context.Context, id string) error
	Rates(ctx context.Context, zoneID string) ([]domain.Rate, error)
	CreateRate(ctx context.Context, r domain.Rate) (domain.Rate, error)
	UpdateRate(ctx context.Context, id string, r domain.Rate) (domain.Rate, error)
	DeleteRate(ctx context.Context, id string) error
	Quote(ctx context.Context, req service.QuoteRequest) (service.Quote, error)
	CreateShipment(ctx context.Context, req service.ShipmentRequest) (domain.Shipment, error)
	Shipments(ctx context.Context, status domain.Status, after *repository.ShipmentCursor, limit int) ([]domain.Shipment, *repository.ShipmentCursor, error)
	Shipment(ctx context.Context, id string) (domain.Shipment, []domain.Event, error)
	Update(ctx context.Context, id string, u domain.Update) (domain.Shipment, error)
	Track(ctx context.Context, tracking string) (domain.Shipment, []domain.Event, error)
}

// NewRouter returns the service's routes on the standard httpkit chain.
// POSTs carrying an Idempotency-Key are replayed from idem.
func NewRouter(svc Shipping, idem httpkit.IdempotencyStore, build httpkit.BuildInfo, checks []httpkit.Check) http.Handler {
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

// Routes are the gateway routes yc-shipping registers (ADR-020 rule 7).
func Routes() []httpkit.RegisteredRoute {
	admin := []string{string(authctx.ActorShopAdmin)}
	return []httpkit.RegisteredRoute{
		{Prefix: "/shipping", Methods: []string{http.MethodGet}},
		{Prefix: "/admin/zones", Roles: admin},
		{Prefix: "/admin/rates", Roles: admin},
		{Prefix: "/admin/shipments", Roles: admin},
		{Prefix: "/internal/quote", Methods: []string{http.MethodPost}},
		{Prefix: "/internal/shipments", Methods: []string{http.MethodPost}},
	}
}

type handlers struct {
	svc Shipping
}

var _ api.StrictServerInterface = handlers{}

func (h handlers) ListPublicZones(ctx context.Context, _ api.ListPublicZonesRequestObject) (api.ListPublicZonesResponseObject, error) {
	zones, err := h.svc.PublicZones(ctx)
	if err != nil {
		return nil, err
	}
	out := api.ListPublicZones200JSONResponse{Items: make([]api.PublicZone, len(zones))}
	for i, z := range zones {
		out.Items[i] = api.PublicZone{ZoneID: mustUUID(z.ID), Name: z.Name, MatchRules: rules(z.Rules)}
	}
	return out, nil
}

func (h handlers) TrackShipment(ctx context.Context, req api.TrackShipmentRequestObject) (api.TrackShipmentResponseObject, error) {
	sh, evs, err := h.svc.Track(ctx, req.TrackingNumber)
	if err != nil {
		return nil, err
	}
	return api.TrackShipment200JSONResponse{
		TrackingNumber: sh.TrackingNumber, Status: api.ShipmentStatus(sh.Status), Method: sh.Method,
		CarrierName: optional(sh.CarrierName), CarrierReference: optional(sh.CarrierReference), Events: trackingEvents(evs),
	}, nil
}

func (h handlers) ListZones(ctx context.Context, _ api.ListZonesRequestObject) (api.ListZonesResponseObject, error) {
	zones, err := h.svc.Zones(ctx)
	if err != nil {
		return nil, err
	}
	out := api.ListZones200JSONResponse{Items: make([]api.Zone, len(zones))}
	for i, z := range zones {
		out.Items[i] = zone(z)
	}
	return out, nil
}

func (h handlers) CreateZone(ctx context.Context, req api.CreateZoneRequestObject) (api.CreateZoneResponseObject, error) {
	z, err := h.svc.CreateZone(ctx, zoneInput(*req.Body))
	if err != nil {
		return nil, err
	}
	return api.CreateZone201JSONResponse(zone(z)), nil
}

func (h handlers) UpdateZone(ctx context.Context, req api.UpdateZoneRequestObject) (api.UpdateZoneResponseObject, error) {
	z, err := h.svc.UpdateZone(ctx, req.ID.String(), zoneInput(*req.Body))
	if err != nil {
		return nil, err
	}
	return api.UpdateZone200JSONResponse(zone(z)), nil
}

func (h handlers) DeleteZone(ctx context.Context, req api.DeleteZoneRequestObject) (api.DeleteZoneResponseObject, error) {
	if err := h.svc.DeleteZone(ctx, req.ID.String()); err != nil {
		return nil, err
	}
	return api.DeleteZone204Response{}, nil
}

func (h handlers) ListRates(ctx context.Context, req api.ListRatesRequestObject) (api.ListRatesResponseObject, error) {
	var zoneID string
	if req.Params.ZoneID != nil {
		zoneID = req.Params.ZoneID.String()
	}
	rates, err := h.svc.Rates(ctx, zoneID)
	if err != nil {
		return nil, err
	}
	out := api.ListRates200JSONResponse{Items: make([]api.Rate, len(rates))}
	for i, r := range rates {
		out.Items[i] = rate(r)
	}
	return out, nil
}

func (h handlers) CreateRate(ctx context.Context, req api.CreateRateRequestObject) (api.CreateRateResponseObject, error) {
	r, err := h.svc.CreateRate(ctx, rateInput(*req.Body))
	if err != nil {
		return nil, err
	}
	return api.CreateRate201JSONResponse(rate(r)), nil
}

func (h handlers) UpdateRate(ctx context.Context, req api.UpdateRateRequestObject) (api.UpdateRateResponseObject, error) {
	r, err := h.svc.UpdateRate(ctx, req.ID.String(), rateInput(*req.Body))
	if err != nil {
		return nil, err
	}
	return api.UpdateRate200JSONResponse(rate(r)), nil
}

func (h handlers) DeleteRate(ctx context.Context, req api.DeleteRateRequestObject) (api.DeleteRateResponseObject, error) {
	if err := h.svc.DeleteRate(ctx, req.ID.String()); err != nil {
		return nil, err
	}
	return api.DeleteRate204Response{}, nil
}

func (h handlers) ListShipments(ctx context.Context, req api.ListShipmentsRequestObject) (api.ListShipmentsResponseObject, error) {
	page, err := httpkit.ParsePage(req.Params.PageSize, req.Params.Cursor)
	if err != nil {
		return nil, err
	}
	var after *repository.ShipmentCursor
	if page.Cursor != "" {
		after = &repository.ShipmentCursor{}
		if err := httpkit.DecodeCursor(page.Cursor, after); err != nil {
			return nil, err
		}
	}
	var status domain.Status
	if req.Params.Status != nil {
		status = domain.Status(*req.Params.Status)
	}
	list, next, err := h.svc.Shipments(ctx, status, after, page.Size)
	if err != nil {
		return nil, err
	}
	info := httpkit.PageInfo{Size: page.Size, Cursor: page.Cursor}
	if next != nil {
		info.NextCursor = httpkit.EncodeCursor(next)
	}
	out := api.ListShipments200JSONResponse{Items: make([]api.Shipment, len(list)), Page: info}
	for i, sh := range list {
		out.Items[i] = shipment(sh)
	}
	return out, nil
}

func (h handlers) GetShipment(ctx context.Context, req api.GetShipmentRequestObject) (api.GetShipmentResponseObject, error) {
	sh, evs, err := h.svc.Shipment(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.GetShipment200JSONResponse(shipmentDetail(sh, evs)), nil
}

func (h handlers) UpdateShipment(ctx context.Context, req api.UpdateShipmentRequestObject) (api.UpdateShipmentResponseObject, error) {
	b := req.Body
	u := domain.Update{
		Status: domain.Status(b.Status), Message: deref(b.Message),
		CarrierName: deref(b.CarrierName), CarrierReference: deref(b.CarrierReference), OccurredAt: deref(b.OccurredAt),
	}
	if _, err := h.svc.Update(ctx, req.ID.String(), u); err != nil {
		return nil, err
	}
	sh, evs, err := h.svc.Shipment(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.UpdateShipment200JSONResponse(shipmentDetail(sh, evs)), nil
}

func (h handlers) QuoteShipping(ctx context.Context, req api.QuoteShippingRequestObject) (api.QuoteShippingResponseObject, error) {
	b := req.Body
	items := make([]service.QuoteItem, len(b.Items))
	for i, it := range b.Items {
		items[i] = service.QuoteItem{
			SKUID: it.SkuID.String(), Quantity: it.Quantity, WeightG: it.WeightGrams, IsDigital: deref(it.IsDigital),
		}
	}
	q, err := h.svc.Quote(ctx, service.QuoteRequest{DeliveryAddress: b.DeliveryAddress, Items: items, SubtotalMinor: b.SubtotalMinor})
	if err != nil {
		return nil, err
	}
	out := api.QuoteShipping200JSONResponse{Options: make([]api.ShippingOption, len(q.Options))}
	if q.Zone != nil {
		out.Zone = &api.QuoteZone{ZoneID: mustUUID(q.Zone.ID), Name: q.Zone.Name}
	}
	for i, o := range q.Options {
		out.Options[i] = api.ShippingOption{
			OptionID: o.ID, RateID: mustUUID(o.RateID), Name: o.Name, AmountMinor: o.AmountMinor,
			EstimatedDaysMin: o.EstimatedDaysMin, EstimatedDaysMax: o.EstimatedDaysMax, CarrierDriver: o.CarrierDriver,
		}
	}
	return out, nil
}

func (h handlers) CreateShipment(ctx context.Context, req api.CreateShipmentRequestObject) (api.CreateShipmentResponseObject, error) {
	b := req.Body
	in := service.ShipmentRequest{
		OrderID: b.OrderID.String(), OrderNumber: b.OrderNumber, Method: b.Method, DeliveryAddress: b.DeliveryAddress,
	}
	if b.RateID != nil {
		in.RateID = b.RateID.String()
	}
	sh, err := h.svc.CreateShipment(ctx, in)
	if err != nil {
		return nil, err
	}
	return api.CreateShipment200JSONResponse(shipment(sh)), nil
}

func zoneInput(b api.ZoneInput) domain.Zone {
	z := domain.Zone{Name: b.Name, Priority: domain.DefaultPriority, Rules: make([]domain.Rule, len(b.MatchRules))}
	if b.Priority != nil {
		z.Priority = *b.Priority
	}
	for i, r := range b.MatchRules {
		z.Rules[i] = domain.Rule{CountryCode: r.CountryCode, Fields: deref(r.Fields)}
	}
	return z
}

func zone(z domain.Zone) api.Zone {
	return api.Zone{
		ZoneID: mustUUID(z.ID), Name: z.Name, MatchRules: rules(z.Rules), Priority: z.Priority,
		CreatedAt: z.CreatedAt.UTC(), UpdatedAt: z.UpdatedAt.UTC(),
	}
}

func rules(rs []domain.Rule) []api.MatchRule {
	out := make([]api.MatchRule, len(rs))
	for i, r := range rs {
		out[i] = api.MatchRule{CountryCode: r.CountryCode}
		if len(r.Fields) > 0 {
			f := r.Fields
			out[i].Fields = &f
		}
	}
	return out
}

func rateInput(b api.RateInput) domain.Rate {
	r := domain.Rate{
		ZoneID: b.ZoneID.String(), Method: b.Method, DisplayName: b.DisplayName, PricingType: domain.PricingType(b.PricingType),
		FlatAmountMinor: deref(b.FlatAmountMinor), EstimatedDaysMin: b.EstimatedDaysMin, EstimatedDaysMax: b.EstimatedDaysMax,
		CarrierDriver: carrier.ManualName, Active: true,
	}
	if b.CarrierDriver != nil {
		r.CarrierDriver = *b.CarrierDriver
	}
	if b.Active != nil {
		r.Active = *b.Active
	}
	if b.WeightTiers != nil {
		for _, t := range *b.WeightTiers {
			r.WeightTiers = append(r.WeightTiers, domain.WeightTier{MaxWeightG: t.MaxWeightG, AmountMinor: t.AmountMinor})
		}
	}
	if b.PriceTiers != nil {
		for _, t := range *b.PriceTiers {
			r.PriceTiers = append(r.PriceTiers, domain.PriceTier{MinSubtotalMinor: t.MinSubtotalMinor, AmountMinor: t.AmountMinor})
		}
	}
	return r
}

func rate(r domain.Rate) api.Rate {
	out := api.Rate{
		RateID: mustUUID(r.ID), ZoneID: mustUUID(r.ZoneID), Method: r.Method, DisplayName: r.DisplayName,
		PricingType: api.PricingType(r.PricingType), FlatAmountMinor: r.FlatAmountMinor,
		WeightTiers: make([]api.WeightTier, len(r.WeightTiers)), PriceTiers: make([]api.PriceTier, len(r.PriceTiers)),
		EstimatedDaysMin: r.EstimatedDaysMin, EstimatedDaysMax: r.EstimatedDaysMax, CarrierDriver: r.CarrierDriver,
		Active: r.Active, CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC(),
	}
	for i, t := range r.WeightTiers {
		out.WeightTiers[i] = api.WeightTier{MaxWeightG: t.MaxWeightG, AmountMinor: t.AmountMinor}
	}
	for i, t := range r.PriceTiers {
		out.PriceTiers[i] = api.PriceTier{MinSubtotalMinor: t.MinSubtotalMinor, AmountMinor: t.AmountMinor}
	}
	return out
}

func shipment(sh domain.Shipment) api.Shipment {
	out := api.Shipment{
		ShipmentID: mustUUID(sh.ID), OrderID: mustUUID(sh.OrderID), OrderNumber: sh.OrderNumber, Method: sh.Method,
		CarrierDriver: sh.CarrierDriver, TrackingNumber: sh.TrackingNumber, LabelURL: optional(sh.LabelURL),
		Status: api.ShipmentStatus(sh.Status), DeliveryAddress: sh.DeliveryAddress,
		CarrierName: optional(sh.CarrierName), CarrierReference: optional(sh.CarrierReference),
		CreatedAt: sh.CreatedAt.UTC(), UpdatedAt: sh.UpdatedAt.UTC(),
	}
	if sh.RateID != "" {
		id := mustUUID(sh.RateID)
		out.RateID = &id
	}
	return out
}

func shipmentDetail(sh domain.Shipment, evs []domain.Event) api.ShipmentDetail {
	s := shipment(sh)
	return api.ShipmentDetail{
		ShipmentID: s.ShipmentID, OrderID: s.OrderID, OrderNumber: s.OrderNumber, Method: s.Method, RateID: s.RateID,
		CarrierDriver: s.CarrierDriver, TrackingNumber: s.TrackingNumber, LabelURL: s.LabelURL, Status: s.Status,
		DeliveryAddress: s.DeliveryAddress, CarrierName: s.CarrierName, CarrierReference: s.CarrierReference,
		CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt, Events: trackingEvents(evs),
	}
}

func trackingEvents(evs []domain.Event) []api.TrackingEvent {
	out := make([]api.TrackingEvent, len(evs))
	for i, e := range evs {
		out[i] = api.TrackingEvent{Status: api.ShipmentStatus(e.Status), Message: optional(e.Message), OccurredAt: e.OccurredAt.UTC()}
	}
	return out
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
