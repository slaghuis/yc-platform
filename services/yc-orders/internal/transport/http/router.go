// Package http is yc-orders' HTTP transport. Routing, parameter parsing
// and request/response types are generated from api/openapi.yaml.
package http

import (
	"bytes"
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
	"github.com/slaghuis/yc-platform/services/yc-orders/internal/service"
)

// Orders is the service the handlers call.
type Orders interface {
	Create(ctx context.Context, o domain.Order) (domain.Order, error)
	Confirm(ctx context.Context, id, paymentTransactionID string) (domain.Order, error)
	Abandon(ctx context.Context, id, reason string) (domain.Order, bool, error)
	Cancel(ctx context.Context, id, reason string) (domain.Order, error)
	Package(ctx context.Context, id string) (domain.Order, error)
	Refund(ctx context.Context, id string, amountMinor int64, reason, key string) (domain.Order, error)
	Order(ctx context.Context, id string) (service.Detail, error)
	LatestPurchase(ctx context.Context, userID, productID string) (repository.Purchase, bool, error)
	CustomerOrders(ctx context.Context, userIDs []string) ([]repository.CustomerOrder, int, error)
	AddressInUse(ctx context.Context, addressID string) (bool, []string, error)
	AdminOrder(ctx context.Context, id string) (service.Detail, error)
	CustomerOrder(ctx context.Context, id, guestToken string) (service.Detail, error)
	MintGuestAccess(ctx context.Context, id string) (token string, exp time.Time, err error)
	InvoicePDF(ctx context.Context, id, guestToken string) (filename string, pdf []byte, err error)
	Mine(ctx context.Context, after *repository.OrderCursor, limit int) ([]domain.Order, error)
	AdminList(ctx context.Context, status domain.Status, after *repository.OrderCursor, limit int) ([]domain.Order, error)
	RequestReturn(ctx context.Context, orderID string, items []domain.ReturnItem, reason string) (domain.Return, error)
	AcceptReturn(ctx context.Context, id string, refundShipping bool, note string) (domain.Return, error)
	RejectReturn(ctx context.Context, id, reason string) (domain.Return, error)
	Returns(ctx context.Context, status domain.ReturnStatus, after *repository.ReturnCursor, limit int) ([]domain.Return, error)
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
			Prefix: "/orders", Methods: []string{http.MethodGet, http.MethodPost},
			Roles: []string{string(authctx.ActorCustomer), string(authctx.ActorVisitor), string(authctx.ActorShopAdmin)},
		},
		{Prefix: "/admin/orders", Roles: admin},
		{Prefix: "/admin/returns", Roles: admin},
		{Prefix: "/internal/orders"},
		{Prefix: "/internal/purchases", Methods: []string{http.MethodGet}},
		{Prefix: "/internal/customer-orders", Methods: []string{http.MethodGet}},
		{Prefix: "/internal/orders/address-in-use", Methods: []string{http.MethodGet}},
	}
}

func (h handlers) ListCustomerOrders(ctx context.Context, req api.ListCustomerOrdersRequestObject) (api.ListCustomerOrdersResponseObject, error) {
	os, open, err := h.svc.CustomerOrders(ctx, req.Params.UserID)
	if err != nil {
		return nil, err
	}
	out := api.ListCustomerOrders200JSONResponse{OpenCount: open, Orders: make([]api.CustomerOrder, len(os))}
	for i, o := range os {
		out.Orders[i] = api.CustomerOrder{OrderID: mustUUID(o.ID), Status: api.OrderStatus(o.Status), OpenReturn: o.OpenReturn}
	}
	return out, nil
}

func (h handlers) AddressInUse(ctx context.Context, req api.AddressInUseRequestObject) (api.AddressInUseResponseObject, error) {
	inUse, ids, err := h.svc.AddressInUse(ctx, req.Params.AddressID.String())
	if err != nil {
		return nil, err
	}
	out := api.AddressInUse200JSONResponse{InUse: inUse}
	if len(ids) > 0 {
		parsed := make([]uuid.UUID, 0, len(ids))
		for _, id := range ids {
			if u, err := uuid.Parse(id); err == nil {
				parsed = append(parsed, u)
			}
		}
		out.OrderIds = &parsed
	}
	return out, nil
}

func (h handlers) FindPurchase(ctx context.Context, req api.FindPurchaseRequestObject) (api.FindPurchaseResponseObject, error) {
	p, ok, err := h.svc.LatestPurchase(ctx, req.Params.UserID.String(), req.Params.ProductID.String())
	if err != nil {
		return nil, err
	}
	out := api.FindPurchase200JSONResponse{Purchased: ok}
	if ok {
		id, at := uuid.MustParse(p.OrderID), p.PlacedAt.UTC()
		out.OrderID, out.OrderNumber, out.PlacedAt = &id, &p.OrderNumber, &at
	}
	return out, nil
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
	token := ""
	if req.Params.GuestToken != nil {
		token = *req.Params.GuestToken
	}
	d, err := h.svc.CustomerOrder(ctx, req.ID.String(), token)
	if err != nil {
		return nil, err
	}
	return api.GetMyOrder200JSONResponse(detail(d)), nil
}

func (h handlers) GetOrderInvoice(ctx context.Context, req api.GetOrderInvoiceRequestObject) (api.GetOrderInvoiceResponseObject, error) {
	token := ""
	if req.Params.GuestToken != nil {
		token = *req.Params.GuestToken
	}
	name, pdf, err := h.svc.InvoicePDF(ctx, req.ID.String(), token)
	if err != nil {
		return nil, err
	}
	disposition := `attachment; filename="` + name + `"`
	return api.GetOrderInvoice200ApplicationPdfResponse{
		Body: bytes.NewReader(pdf), ContentLength: int64(len(pdf)),
		Headers: api.GetOrderInvoice200ResponseHeaders{ContentDisposition: &disposition},
	}, nil
}

func (h handlers) MintGuestAccess(ctx context.Context, req api.MintGuestAccessRequestObject) (api.MintGuestAccessResponseObject, error) {
	token, exp, err := h.svc.MintGuestAccess(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.MintGuestAccess200JSONResponse{Token: token, ExpiresAt: exp.UTC()}, nil
}

func (h handlers) RequestReturn(ctx context.Context, req api.RequestReturnRequestObject) (api.RequestReturnResponseObject, error) {
	items := make([]domain.ReturnItem, len(req.Body.Items))
	for i, it := range req.Body.Items {
		items[i] = domain.ReturnItem{SKUID: it.SkuID.String(), Quantity: it.Quantity}
	}
	r, err := h.svc.RequestReturn(ctx, req.ID.String(), items, deref(req.Body.Reason))
	if err != nil {
		return nil, err
	}
	return api.RequestReturn201JSONResponse(returnOut(r)), nil
}

func (h handlers) PackageOrder(ctx context.Context, req api.PackageOrderRequestObject) (api.PackageOrderResponseObject, error) {
	o, err := h.svc.Package(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.PackageOrder200JSONResponse(order(o, nil)), nil
}

func (h handlers) RefundOrder(ctx context.Context, req api.RefundOrderRequestObject) (api.RefundOrderResponseObject, error) {
	o, err := h.svc.Refund(ctx, req.ID.String(), req.Body.AmountMinor, req.Body.Reason, req.Params.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	return api.RefundOrder200JSONResponse(order(o, nil)), nil
}

func (h handlers) ListReturns(ctx context.Context, req api.ListReturnsRequestObject) (api.ListReturnsResponseObject, error) {
	page, err := httpkit.ParsePage(req.Params.PageSize, req.Params.Cursor)
	if err != nil {
		return nil, err
	}
	var after *repository.ReturnCursor
	if page.Cursor != "" {
		after = &repository.ReturnCursor{}
		if err := httpkit.DecodeCursor(page.Cursor, after); err != nil {
			return nil, err
		}
	}
	var status domain.ReturnStatus
	if req.Params.Status != nil {
		status = domain.ReturnStatus(*req.Params.Status)
	}
	rs, err := h.svc.Returns(ctx, status, after, page.Size+1)
	if err != nil {
		return nil, err
	}
	info := httpkit.PageInfo{Size: page.Size, Cursor: page.Cursor}
	if len(rs) > page.Size {
		rs = rs[:page.Size]
		last := rs[len(rs)-1]
		info.NextCursor = httpkit.EncodeCursor(repository.ReturnCursor{At: last.RequestedAt, ID: last.ID})
	}
	out := api.ListReturns200JSONResponse{Items: make([]api.Return, len(rs)), Page: info}
	for i, r := range rs {
		out.Items[i] = returnOut(r)
	}
	return out, nil
}

func (h handlers) AcceptReturn(ctx context.Context, req api.AcceptReturnRequestObject) (api.AcceptReturnResponseObject, error) {
	var refundShipping bool
	var note string
	if req.Body != nil {
		refundShipping, note = deref(req.Body.RefundShipping), deref(req.Body.Note)
	}
	r, err := h.svc.AcceptReturn(ctx, req.ID.String(), refundShipping, note)
	if err != nil {
		return nil, err
	}
	return api.AcceptReturn200JSONResponse(returnOut(r)), nil
}

func (h handlers) RejectReturn(ctx context.Context, req api.RejectReturnRequestObject) (api.RejectReturnResponseObject, error) {
	r, err := h.svc.RejectReturn(ctx, req.ID.String(), req.Body.Reason)
	if err != nil {
		return nil, err
	}
	return api.RejectReturn200JSONResponse(returnOut(r)), nil
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
	d, err := h.svc.AdminOrder(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.GetOrder200JSONResponse(detail(d)), nil
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
	if b.DeliveryAddressID != nil {
		o.DeliveryAddressID = b.DeliveryAddressID.String()
	}
	if b.BillingAddress != nil {
		ba := billingIn(*b.BillingAddress)
		o.BillingAddress = &ba
	}
	if b.ShippingOption != nil {
		if o.ShippingSnapshot, err = json.Marshal(*b.ShippingOption); err != nil {
			return nil, err
		}
	}
	if b.DeliverySlot != nil {
		ds := domain.NormaliseDeliverySlot(domain.DeliverySlot{
			SlotID: b.DeliverySlot.SlotID, StartAt: b.DeliverySlot.StartAt, EndAt: b.DeliverySlot.EndAt,
			Timezone: b.DeliverySlot.Timezone, Label: deref(b.DeliverySlot.Label),
		})
		if b.DeliverySlot.ScheduleID != nil {
			ds.ScheduleID = b.DeliverySlot.ScheduleID.String()
		}
		o.DeliverySlot = &ds
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
		if it.Components != nil {
			item.Components = make([]domain.ComponentSnapshot, len(*it.Components))
			for j, c := range *it.Components {
				item.Components[j] = domain.ComponentSnapshot{
					SKUID: c.SkuID.String(), Quantity: c.Quantity,
					ProductName: deref(c.ProductName), SKUCode: deref(c.SkuCode),
				}
			}
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
	d, err := h.svc.Order(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.GetInternalOrder200JSONResponse(detail(d)), nil
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
	if o.DeliveryAddressID != "" {
		if id, err := uuid.Parse(o.DeliveryAddressID); err == nil {
			out.DeliveryAddressID = &id
		}
	}
	if o.BillingAddress != nil {
		ba := billingOut(*o.BillingAddress)
		out.BillingAddress = &ba
	}
	if o.SellerInvoice != nil {
		si := sellerOut(*o.SellerInvoice)
		out.SellerInvoice = &si
	}
	if ds := o.DeliverySlot; ds != nil {
		slot := api.DeliverySlot{
			SlotID: ds.SlotID, StartAt: ds.StartAt.UTC(), EndAt: ds.EndAt.UTC(), Timezone: ds.Timezone,
			Label: optional(ds.Label),
		}
		if ds.ScheduleID != "" {
			if id, err := uuid.Parse(ds.ScheduleID); err == nil {
				slot.ScheduleID = &id
			}
		}
		out.DeliverySlot = &slot
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
		if len(it.Components) > 0 {
			comps := make([]api.LineComponent, len(it.Components))
			for j, c := range it.Components {
				comps[j] = api.LineComponent{SkuID: mustUUID(c.SKUID), Quantity: c.Quantity}
				if c.ProductName != "" {
					n := c.ProductName
					comps[j].ProductName = &n
				}
				if c.SKUCode != "" {
					code := c.SKUCode
					comps[j].SkuCode = &code
				}
			}
			item.Components = &comps
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
	if c := o.ShippingChoice(); o.RequiresShipping {
		out.ShippingMethod = &c.Method
	}
	if sh := o.Shipment; sh.ID != "" {
		out.Shipment = &api.OrderShipment{
			ShipmentID: mustUUID(sh.ID), TrackingNumber: sh.TrackingNumber, CarrierName: optional(sh.CarrierName),
			CarrierReference: optional(sh.CarrierReference), PackagedAt: utc(sh.PackagedAt), ShippedAt: utc(sh.ShippedAt),
			DeliveredAt: utc(sh.DeliveredAt),
		}
	}
	return out
}

// detail is an order with its history, returns and refunds.
func detail(d service.Detail) api.Order {
	out := order(d.Order, d.History)
	rs := make([]api.Return, len(d.Returns))
	for i, r := range d.Returns {
		rs[i] = returnOut(r)
	}
	out.Returns = &rs
	fs := make([]api.Refund, len(d.Refunds))
	for i, f := range d.Refunds {
		fs[i] = api.Refund{
			RefundID: mustUUID(f.ID), Amount: money(f.AmountMinor, d.Order.Currency),
			Status: api.RefundStatus(f.Status), Reason: f.Reason, CreatedAt: f.CreatedAt.UTC(),
		}
		if f.ReturnID != "" {
			id := mustUUID(f.ReturnID)
			fs[i].ReturnID = &id
		}
	}
	out.Refunds = &fs
	return out
}

func returnOut(r domain.Return) api.Return {
	items := make([]api.ReturnItem, len(r.Items))
	for i, it := range r.Items {
		items[i] = api.ReturnItem{SkuID: mustUUID(it.SKUID), Quantity: it.Quantity}
	}
	return api.Return{
		ReturnID: mustUUID(r.ID), OrderID: mustUUID(r.OrderID), Status: api.ReturnStatus(r.Status), Items: items,
		Reason: optional(r.Reason), RefundShipping: r.RefundShipping, Refund: r.RefundMinor,
		DecisionReason: optional(r.DecisionReason), RequestedAt: r.RequestedAt.UTC(), DecidedAt: utc(r.DecidedAt),
	}
}

func mustUUID(s string) uuid.UUID {
	id, _ := uuid.Parse(s)
	return id
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

func billingIn(b api.BillingAddress) domain.BillingAddress {
	return domain.BillingAddress{
		CompanyName: deref(b.CompanyName), VATNumber: deref(b.VatNumber),
		StreetAndNumber: b.StreetAndNumber, Suburb: deref(b.Suburb),
		TownCity: b.TownCity, PostalCode: b.PostalCode, CountryCode: deref(b.CountryCode),
	}
}

func billingOut(b domain.BillingAddress) api.BillingAddress {
	out := api.BillingAddress{
		StreetAndNumber: b.StreetAndNumber, TownCity: b.TownCity, PostalCode: b.PostalCode,
	}
	if b.CompanyName != "" {
		out.CompanyName = &b.CompanyName
	}
	if b.VATNumber != "" {
		out.VatNumber = &b.VATNumber
	}
	if b.Suburb != "" {
		out.Suburb = &b.Suburb
	}
	if b.CountryCode != "" {
		out.CountryCode = &b.CountryCode
	}
	return out
}

func sellerOut(s domain.SellerInvoice) api.SellerInvoice {
	out := api.SellerInvoice{Name: s.Name, ContactEmail: s.ContactEmail}
	if s.VATNumber != "" {
		out.VatNumber = &s.VATNumber
	}
	if s.InvoiceAddress != nil {
		a := billingOut(*s.InvoiceAddress)
		out.InvoiceAddress = &a
	}
	return out
}
