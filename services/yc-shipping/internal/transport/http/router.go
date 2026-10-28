// Package http is yc-shipping's HTTP transport. Routing, parameter parsing
// and request/response types are generated from api/openapi.yaml.
package http

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
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
	Schedules(ctx context.Context) ([]domain.Schedule, error)
	Schedule(ctx context.Context, id string) (domain.Schedule, error)
	CreateSchedule(ctx context.Context, sc domain.Schedule) (domain.Schedule, error)
	UpdateSchedule(ctx context.Context, id string, sc domain.Schedule) (domain.Schedule, error)
	DeleteSchedule(ctx context.Context, id string) error
	ListSlots(ctx context.Context, rateID string, from, to time.Time) ([]domain.Slot, error)
	GetSlot(ctx context.Context, slotID string) (domain.Slot, error)
	BookSlot(ctx context.Context, slotID, bookingKey string) (domain.Slot, error)
	ReleaseSlot(ctx context.Context, slotID, bookingKey string) error
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
		{Prefix: "/admin/shipping/schedules", Roles: admin},
		{Prefix: "/admin/shipments", Roles: admin},
		{Prefix: "/internal/quote", Methods: []string{http.MethodPost}},
		{Prefix: "/internal/slots", Methods: []string{http.MethodGet, http.MethodPost}},
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

func (h handlers) ListSchedules(ctx context.Context, _ api.ListSchedulesRequestObject) (api.ListSchedulesResponseObject, error) {
	list, err := h.svc.Schedules(ctx)
	if err != nil {
		return nil, err
	}
	out := api.ListSchedules200JSONResponse{Items: make([]api.Schedule, len(list))}
	for i, sc := range list {
		out.Items[i] = schedule(sc)
	}
	return out, nil
}

func (h handlers) CreateSchedule(ctx context.Context, req api.CreateScheduleRequestObject) (api.CreateScheduleResponseObject, error) {
	sc, err := h.svc.CreateSchedule(ctx, scheduleInput(*req.Body))
	if err != nil {
		return nil, err
	}
	return api.CreateSchedule201JSONResponse(schedule(sc)), nil
}

func (h handlers) GetSchedule(ctx context.Context, req api.GetScheduleRequestObject) (api.GetScheduleResponseObject, error) {
	sc, err := h.svc.Schedule(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.GetSchedule200JSONResponse(schedule(sc)), nil
}

func (h handlers) UpdateSchedule(ctx context.Context, req api.UpdateScheduleRequestObject) (api.UpdateScheduleResponseObject, error) {
	sc, err := h.svc.UpdateSchedule(ctx, req.ID.String(), scheduleInput(*req.Body))
	if err != nil {
		return nil, err
	}
	return api.UpdateSchedule200JSONResponse(schedule(sc)), nil
}

func (h handlers) DeleteSchedule(ctx context.Context, req api.DeleteScheduleRequestObject) (api.DeleteScheduleResponseObject, error) {
	if err := h.svc.DeleteSchedule(ctx, req.ID.String()); err != nil {
		return nil, err
	}
	return api.DeleteSchedule204Response{}, nil
}

func (h handlers) ListSlots(ctx context.Context, req api.ListSlotsRequestObject) (api.ListSlotsResponseObject, error) {
	from, to, err := slotRange(req.Params.From, req.Params.To)
	if err != nil {
		return nil, err
	}
	slots, err := h.svc.ListSlots(ctx, req.Params.RateID.String(), from, to)
	if err != nil {
		return nil, err
	}
	out := api.ListSlots200JSONResponse{Slots: make([]api.Slot, len(slots))}
	for i, sl := range slots {
		out.Slots[i] = slot(sl)
	}
	return out, nil
}

func (h handlers) GetSlot(ctx context.Context, req api.GetSlotRequestObject) (api.GetSlotResponseObject, error) {
	sl, err := h.svc.GetSlot(ctx, req.SlotID)
	if err != nil {
		return nil, err
	}
	return api.GetSlot200JSONResponse(slot(sl)), nil
}

func (h handlers) BookSlot(ctx context.Context, req api.BookSlotRequestObject) (api.BookSlotResponseObject, error) {
	sl, err := h.svc.BookSlot(ctx, req.SlotID, req.Body.BookingKey)
	if err != nil {
		return nil, err
	}
	return api.BookSlot200JSONResponse(slot(sl)), nil
}

func (h handlers) ReleaseSlot(ctx context.Context, req api.ReleaseSlotRequestObject) (api.ReleaseSlotResponseObject, error) {
	if err := h.svc.ReleaseSlot(ctx, req.SlotID, req.Body.BookingKey); err != nil {
		return nil, err
	}
	return api.ReleaseSlot204Response{}, nil
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
		opt := api.ShippingOption{
			OptionID: o.ID, RateID: mustUUID(o.RateID), Name: o.Name, AmountMinor: o.AmountMinor,
			EstimatedDaysMin: o.EstimatedDaysMin, EstimatedDaysMax: o.EstimatedDaysMax, CarrierDriver: o.CarrierDriver,
			RequiresDeliverySlot: o.RequiresDeliverySlot,
		}
		if o.ScheduleID != "" {
			id := mustUUID(o.ScheduleID)
			opt.ScheduleID = &id
		}
		out.Options[i] = opt
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
		CarrierDriver: carrier.ManualName, Active: true, RequiresDeliverySlot: deref(b.RequiresDeliverySlot),
	}
	if b.CarrierDriver != nil {
		r.CarrierDriver = *b.CarrierDriver
	}
	if b.Active != nil {
		r.Active = *b.Active
	}
	if b.ScheduleID != nil {
		r.ScheduleID = b.ScheduleID.String()
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
		RequiresDeliverySlot: r.RequiresDeliverySlot, Active: r.Active,
		CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC(),
	}
	if r.ScheduleID != "" {
		id := mustUUID(r.ScheduleID)
		out.ScheduleID = &id
	}
	for i, t := range r.WeightTiers {
		out.WeightTiers[i] = api.WeightTier{MaxWeightG: t.MaxWeightG, AmountMinor: t.AmountMinor}
	}
	for i, t := range r.PriceTiers {
		out.PriceTiers[i] = api.PriceTier{MinSubtotalMinor: t.MinSubtotalMinor, AmountMinor: t.AmountMinor}
	}
	return out
}

func scheduleInput(b api.ScheduleInput) domain.Schedule {
	sc := domain.Schedule{
		Name: b.Name, WindowMinutes: b.WindowMinutes, Weekdays: append([]int(nil), b.Weekdays...),
		OpenTime: b.OpenTime, CloseTime: b.CloseTime, CapacityPerWindow: b.CapacityPerWindow,
		MinLeadMinutes: deref(b.MinLeadMinutes), SameDayCutoff: b.SameDayCutoff, Active: true,
	}
	if b.Timezone != nil {
		sc.Timezone = *b.Timezone
	}
	if b.Active != nil {
		sc.Active = *b.Active
	}
	if b.BlackoutDates != nil {
		sc.BlackoutDates = make([]string, len(*b.BlackoutDates))
		for i, d := range *b.BlackoutDates {
			sc.BlackoutDates[i] = d.Format("2006-01-02")
		}
	}
	return sc
}

func schedule(sc domain.Schedule) api.Schedule {
	out := api.Schedule{
		ScheduleID: mustUUID(sc.ID), Name: sc.Name, Timezone: sc.Timezone, WindowMinutes: sc.WindowMinutes,
		Weekdays: append([]int(nil), sc.Weekdays...), OpenTime: sc.OpenTime, CloseTime: sc.CloseTime,
		CapacityPerWindow: sc.CapacityPerWindow, MinLeadMinutes: sc.MinLeadMinutes, SameDayCutoff: sc.SameDayCutoff,
		Active: sc.Active, CreatedAt: sc.CreatedAt.UTC(), UpdatedAt: sc.UpdatedAt.UTC(),
		BlackoutDates: make([]openapi_types.Date, len(sc.BlackoutDates)),
	}
	for i, d := range sc.BlackoutDates {
		t, _ := time.Parse("2006-01-02", d)
		out.BlackoutDates[i] = openapi_types.Date{Time: t}
	}
	return out
}

func slot(sl domain.Slot) api.Slot {
	return api.Slot{
		SlotID: sl.SlotID, ScheduleID: mustUUID(sl.ScheduleID), StartAt: sl.StartAt.UTC(), EndAt: sl.EndAt.UTC(),
		Timezone: sl.Timezone, RemainingCapacity: sl.RemainingCapacity,
	}
}

func slotRange(from, to openapi_types.Date) (time.Time, time.Time, error) {
	if from.IsZero() || to.IsZero() {
		return time.Time{}, time.Time{}, ycerrors.Validation().
			Field("from", "required", "From and to dates are required").Build()
	}
	return from.Time, to.Time, nil
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
