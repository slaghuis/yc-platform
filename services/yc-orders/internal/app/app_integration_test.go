//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	testkit "github.com/slaghuis/yc-platform/libs/yc-testkit"

	api "github.com/slaghuis/yc-platform/services/yc-orders/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-orders/internal/app"
)

type client struct {
	t   *testing.T
	srv *httptest.Server
	hdr map[string]string
}

func as(t *testing.T, srv *httptest.Server, tenant, actorID, actorType string) client {
	return client{t: t, srv: srv, hdr: map[string]string{
		authctx.HeaderTenantID:  tenant,
		authctx.HeaderActorID:   actorID,
		authctx.HeaderActorType: actorType,
	}}
}

func checkout(t *testing.T, srv *httptest.Server, tenant string) client {
	c := as(t, srv, tenant, "yc-checkout", "service")
	c.hdr[authctx.HeaderCallerService] = "yc-checkout"
	return c
}

func (c client) do(method, path string, body, out any, header ...string) (int, http.Header) {
	c.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(c.t, json.NewEncoder(&buf).Encode(body))
	}
	req, err := http.NewRequestWithContext(context.Background(), method, c.srv.URL+path, &buf)
	require.NoError(c.t, err)
	for k, v := range c.hdr {
		req.Header.Set(k, v)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := c.srv.Client().Do(req)
	require.NoError(c.t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(c.t, err)
	if out != nil && resp.StatusCode < 300 {
		require.NoError(c.t, json.Unmarshal(b, out), string(b))
	}
	return resp.StatusCode, resp.Header
}

func (c client) code(method, path string, body any, header ...string) int {
	c.t.Helper()
	code, _ := c.do(method, path, body, nil, header...)
	return code
}

// fakePayment stands in for yc-gateway's internal listener in front of
// yc-payment's refunds and yc-shipping's shipments.
type fakePayment struct {
	mu        sync.Mutex
	keys      map[string]int64 // Idempotency-Key → amount refunded
	calls     int
	refuses   bool
	shipments map[string]map[string]any // order ID → shipment
	booked    []map[string]any          // shipment requests
}

func (f *fakePayment) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path != "/internal/service-token" &&
		(r.Header.Get("Authorization") != "Bearer svc-yc-orders" || r.Header.Get(authctx.HeaderTenantID) == "") {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/internal/service-token":
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "svc-yc-orders", "expires_at": time.Now().Add(time.Hour)})
	case "/internal/refund":
		var in struct {
			AmountMinor int64 `json:"amount_minor"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.calls++
		if f.refuses {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"type":"about:blank","title":"Invalid state transition","status":409,"code":"invalid_state_transition","detail":"Nothing is left to refund"}`))
			return
		}
		key := r.Header.Get(httpkit.HeaderIdempotencyKey)
		if _, ok := f.keys[key]; !ok {
			f.keys[key] = 11500
			if in.AmountMinor > 0 {
				f.keys[key] = in.AmountMinor
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "completed", "refund": map[string]any{"amount_minor": f.keys[key]},
		})
	case "/internal/shipments":
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.booked = append(f.booked, in)
		orderID, _ := in["order_id"].(string)
		sh, ok := f.shipments[orderID]
		if !ok {
			sh = map[string]any{"shipment_id": uuid.NewString(), "tracking_number": "YC" + uuid.NewString()[:12], "status": "label_created"}
			f.shipments[orderID] = sh
		}
		_ = json.NewEncoder(w).Encode(sh)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestOrders(t *testing.T) {
	ctx := context.Background()
	logs := telemetry.InitForTest(t, "yc-orders")
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	})

	pay := &fakePayment{keys: map[string]int64{}, shipments: map[string]map[string]any{}}
	gw := httptest.NewServer(pay)
	t.Cleanup(gw.Close)
	dbURL := testkit.Postgres(t)
	t.Setenv("YC_ORDERS_DB_URL", dbURL)
	natsURL := testkit.NATS(t)
	t.Setenv("NATS_URL", natsURL)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("YC_GATEWAY_INTERNAL_URL", gw.URL)
	t.Setenv("YC_ORDERS_SERVICE_SECRET", "orders-secret")
	cfg, err := app.LoadConfig()
	require.NoError(t, err)
	cfg.Gateway.InternalURL = ""
	require.NoError(t, cfg.Validate())
	a, err := app.New(ctx, cfg, httpkit.BuildInfo{Service: "yc-orders", Version: "test"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	srv := httptest.NewServer(a.Handler)
	t.Cleanup(srv.Close)

	db, err := pgxpool.New(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(db.Close)
	outbox := func(eventType string) []map[string]any {
		t.Helper()
		rows, err := db.Query(ctx, `SELECT envelope->'payload' FROM outbox
			WHERE envelope->>'event_type' = $1 ORDER BY created_at, id`, eventType)
		require.NoError(t, err)
		defer rows.Close()
		var out []map[string]any
		for rows.Next() {
			var p map[string]any
			require.NoError(t, rows.Scan(&p))
			out = append(out, p)
		}
		require.NoError(t, rows.Err())
		return out
	}

	svc := checkout(t, srv, "shop-a")
	admin := as(t, srv, "shop-a", uuid.NewString(), "shop_admin")
	customerID := uuid.NewString()
	customer := as(t, srv, "shop-a", customerID, "customer")
	mug, ebook, mugProduct := uuid.New(), uuid.New(), uuid.New()
	tx, rateID := uuid.New(), uuid.New()
	newOrder := func(userID, userType string) api.CreateOrderInput {
		digital := true
		code := "MUG-1"
		return api.CreateOrderInput{
			CheckoutSessionID: uuid.New(), UserID: userID, UserType: api.CreateOrderInputUserType(userType),
			RequiresShipping: true, DeliveryAddress: &map[string]any{"line1": "1 Long St", "city": "Cape Town"},
			Items: []api.OrderItemInput{
				{
					SkuID: mug, ProductID: &mugProduct, SkuCode: &code, NameSnapshot: "Mug", Quantity: 2,
					UnitPriceMinor: 5000, LineTotalMinor: 10000,
				},
				{SkuID: ebook, NameSnapshot: "E-book", Quantity: 1, UnitPriceMinor: 500, LineTotalMinor: 500, IsDigital: &digital},
			},
			Currency: "ZAR", ItemsTotalMinor: 10500, ShippingMinor: 1000, TaxTotalMinor: 1370, TotalMinor: 11500,
			PricingSnapshot: map[string]any{"quote_id": "q1"},
			ShippingOption:  &map[string]any{"id": "standard", "rate_id": rateID.String(), "amount_minor": 1000},
			Payment:         &api.Payment{Gateway: "stripe", TransactionID: &tx},
		}
	}
	create := func(in api.CreateOrderInput) api.Order {
		t.Helper()
		var o api.Order
		code, _ := svc.do("POST", "/internal/orders", in, &o, "Idempotency-Key", in.CheckoutSessionID.String()+":order")
		require.Equal(t, http.StatusCreated, code)
		return o
	}
	confirm := func(id uuid.UUID) (int, api.Order) {
		t.Helper()
		var o api.Order
		code, _ := svc.do("POST", "/internal/orders/"+id.String()+"/confirm", api.ConfirmInput{PaymentTransactionID: &tx}, &o,
			"Idempotency-Key", id.String()+":confirm:"+uuid.NewString())
		return code, o
	}

	var first api.Order
	t.Run("a pending order is hidden until confirmed", func(t *testing.T) {
		in := newOrder(customerID, "customer")
		first = create(in)
		assert.Equal(t, api.OrderStatus("pending_payment"), first.Status)
		assert.Nil(t, first.OrderNumber)

		var again api.Order
		code, _ := svc.do("POST", "/internal/orders", in, &again, "Idempotency-Key", "another-key")
		require.Equal(t, http.StatusCreated, code)
		assert.Equal(t, first.OrderID, again.OrderID, "one order per checkout session")
		_, hdr := svc.do("POST", "/internal/orders", in, nil, "Idempotency-Key", in.CheckoutSessionID.String()+":order")
		assert.Equal(t, "true", hdr.Get(httpkit.HeaderIdempotentReplayed))

		assert.Equal(t, http.StatusNotFound, customer.code("GET", "/orders/"+first.OrderID.String(), nil))
		var mine api.OrderList
		code, _ = customer.do("GET", "/orders/mine", nil, &mine)
		require.Equal(t, http.StatusOK, code)
		assert.Empty(t, mine.Items)
		assert.Empty(t, outbox(events.TypeOrderPlacedV1), "nothing is published before confirmation")
	})

	t.Run("checkout input is validated", func(t *testing.T) {
		in := newOrder(uuid.NewString(), "visitor")
		assert.Equal(t, http.StatusBadRequest, svc.code("POST", "/internal/orders", in, "Idempotency-Key", uuid.NewString()),
			"a guest order needs contact details")
		in = newOrder(customerID, "customer")
		in.ItemsTotalMinor = 1
		assert.Equal(t, http.StatusBadRequest, svc.code("POST", "/internal/orders", in, "Idempotency-Key", uuid.NewString()))
		assert.Equal(t, http.StatusUnauthorized, customer.code("POST", "/internal/orders", newOrder(customerID, "customer")),
			"internal routes need a service caller")
	})

	t.Run("confirming places the order with the next number", func(t *testing.T) {
		code, o := confirm(first.OrderID)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, api.OrderStatus("received"), o.Status)
		require.NotNil(t, o.OrderNumber)
		assert.Equal(t, "SHOP-A-000001", *o.OrderNumber)
		code, o = confirm(first.OrderID)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, "SHOP-A-000001", *o.OrderNumber, "confirming again keeps the number")

		placed := outbox(events.TypeOrderPlacedV1)
		require.Len(t, placed, 1)
		assert.Equal(t, "SHOP-A-000001", placed[0]["order_number"])
		assert.Equal(t, tx.String(), placed[0]["payment_transaction_id"])
		assert.Len(t, placed[0]["items"], 2)

		var mine api.OrderList
		code, _ = customer.do("GET", "/orders/mine", nil, &mine)
		require.Equal(t, http.StatusOK, code)
		require.Len(t, mine.Items, 1)
		assert.EqualValues(t, 3, mine.Items[0].ItemCount)
		var got api.Order
		code, _ = customer.do("GET", "/orders/"+first.OrderID.String(), nil, &got)
		require.Equal(t, http.StatusOK, code)
		require.NotNil(t, got.History)
		assert.Len(t, *got.History, 2)
		assert.EqualValues(t, 11500, got.Total.AmountMinor)

		other := as(t, srv, "shop-a", uuid.NewString(), "customer")
		assert.Equal(t, http.StatusNotFound, other.code("GET", "/orders/"+first.OrderID.String(), nil))
		shopB := checkout(t, srv, "shop-b")
		assert.Equal(t, http.StatusNotFound, shopB.code("GET", "/internal/orders/"+first.OrderID.String(), nil))
	})

	t.Run("services check a customer's purchase of a product", func(t *testing.T) {
		purchase := func(userID string, productID uuid.UUID) api.Purchase {
			t.Helper()
			var p api.Purchase
			code, _ := svc.do("GET", "/internal/purchases?user_id="+userID+"&product_id="+productID.String(), nil, &p)
			require.Equal(t, http.StatusOK, code)
			return p
		}
		p := purchase(customerID, mugProduct)
		assert.True(t, p.Purchased)
		require.NotNil(t, p.OrderID)
		assert.Equal(t, first.OrderID, *p.OrderID)
		assert.Equal(t, "SHOP-A-000001", *p.OrderNumber)
		assert.False(t, purchase(customerID, uuid.New()).Purchased, "another product")
		assert.False(t, purchase(uuid.NewString(), mugProduct).Purchased, "another customer")
		assert.Equal(t, http.StatusBadRequest, svc.code("GET", "/internal/purchases?user_id=x&product_id=y", nil))
	})

	t.Run("compensation abandons a pending order", func(t *testing.T) {
		o := create(newOrder(customerID, "customer"))
		var res api.AbandonResult
		code, _ := svc.do("POST", "/internal/orders/"+o.OrderID.String()+"/abandon", api.ReasonInput{}, &res,
			"Idempotency-Key", o.OrderID.String()+":abandon")
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, api.AbandonResultStatus("abandoned"), res.Status)
		code, _ = svc.do("POST", "/internal/orders/"+o.OrderID.String()+"/abandon", nil, &res,
			"Idempotency-Key", uuid.NewString())
		require.Equal(t, http.StatusOK, code, "abandoning again")
		code, _ = confirm(o.OrderID)
		assert.Equal(t, http.StatusConflict, code)

		code, _ = svc.do("POST", "/internal/orders/"+uuid.NewString()+"/abandon", nil, &res, "Idempotency-Key", uuid.NewString())
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, api.AbandonResultStatus("none"), res.Status, "nothing to undo")
		assert.Equal(t, http.StatusConflict, svc.code("POST", "/internal/orders/"+first.OrderID.String()+"/abandon", nil,
			"Idempotency-Key", uuid.NewString()), "a placed order cannot be abandoned")

		var list api.OrderList
		code, _ = admin.do("GET", "/admin/orders", nil, &list)
		require.Equal(t, http.StatusOK, code)
		assert.Len(t, list.Items, 1, "abandoned orders are not listed")
		code, _ = admin.do("GET", "/admin/orders?status=abandoned", nil, &list)
		require.Equal(t, http.StatusOK, code)
		assert.Len(t, list.Items, 1)
	})

	t.Run("a visitor sees a guest order with the same token", func(t *testing.T) {
		visitorID := uuid.NewString()
		in := newOrder(visitorID, "visitor")
		in.GuestContact = &api.GuestContact{Email: "guest@example.com", PhoneE164: "+27821234567"}
		o := create(in)
		code, placed := confirm(o.OrderID)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, "SHOP-A-000002", *placed.OrderNumber, "abandoned orders leave no gap")

		visitor := as(t, srv, "shop-a", visitorID, "visitor")
		var got api.Order
		code, _ = visitor.do("GET", "/orders/"+o.OrderID.String(), nil, &got)
		require.Equal(t, http.StatusOK, code)
		require.NotNil(t, got.GuestContact)
		assert.Equal(t, "guest@example.com", got.GuestContact.Email)
		assert.Equal(t, http.StatusForbidden, visitor.code("GET", "/orders/mine", nil), "/orders/mine is for customers")
		other := as(t, srv, "shop-a", uuid.NewString(), "visitor")
		assert.Equal(t, http.StatusNotFound, other.code("GET", "/orders/"+o.OrderID.String(), nil))
		ev := outbox(events.TypeOrderPlacedV1)
		assert.Equal(t, "guest@example.com", ev[len(ev)-1]["guest_contact"].(map[string]any)["email"])
	})

	t.Run("cancelling refunds through yc-payment", func(t *testing.T) {
		pay.mu.Lock()
		pay.refuses = true
		pay.mu.Unlock()
		code, _ := admin.do("POST", "/admin/orders/"+first.OrderID.String()+"/cancel", api.ReasonInput{}, nil)
		assert.Equal(t, http.StatusConflict, code, "a refused refund leaves the order")
		var o api.Order
		code, _ = admin.do("GET", "/admin/orders/"+first.OrderID.String(), nil, &o)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, api.OrderStatus("received"), o.Status)

		pay.mu.Lock()
		pay.refuses = false
		pay.calls = 0
		pay.mu.Unlock()
		reason := "customer changed their mind"
		code, _ = admin.do("POST", "/admin/orders/"+first.OrderID.String()+"/cancel", api.ReasonInput{Reason: &reason}, &o)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, api.OrderStatus("cancelled"), o.Status)
		assert.EqualValues(t, 11500, o.Refunded.AmountMinor)
		code, _ = admin.do("POST", "/admin/orders/"+first.OrderID.String()+"/cancel", nil, &o)
		require.Equal(t, http.StatusOK, code, "cancelling again")
		pay.mu.Lock()
		assert.Equal(t, 1, pay.calls, "refunded once")
		_, keyed := pay.keys["order:"+first.OrderID.String()+":cancel"]
		pay.mu.Unlock()
		assert.True(t, keyed)

		cancelled := outbox(events.TypeOrderCancelledV1)
		require.Len(t, cancelled, 1)
		assert.Equal(t, reason, cancelled[0]["reason"])
		assert.EqualValues(t, 11500, cancelled[0]["refund"].(map[string]any)["amount_minor"])
		assert.Equal(t, http.StatusForbidden, customer.code("POST", "/admin/orders/"+first.OrderID.String()+"/cancel", nil))
	})

	t.Run("admins page through orders", func(t *testing.T) {
		var page api.OrderList
		code, _ := admin.do("GET", "/admin/orders?page_size=1", nil, &page)
		require.Equal(t, http.StatusOK, code)
		require.Len(t, page.Items, 1)
		require.NotEmpty(t, page.Page.NextCursor)
		var page2 api.OrderList
		code, _ = admin.do("GET", "/admin/orders?page_size=1&cursor="+page.Page.NextCursor, nil, &page2)
		require.Equal(t, http.StatusOK, code)
		require.Len(t, page2.Items, 1)
		assert.NotEqual(t, page.Items[0].OrderID, page2.Items[0].OrderID)
		assert.Empty(t, page2.Page.NextCursor)
	})

	validator, err := events.NewValidator()
	require.NoError(t, err)
	shipping, err := eventbus.NewClient(ctx, eventbus.Config{
		NATSURL: natsURL, ServiceName: "yc-shipping", Validator: validator,
		Streams: []eventbus.StreamConfig{eventbus.ShippingStream()},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = shipping.Close(context.Background()) })
	publish := func(eventType string, payload any) {
		t.Helper()
		require.NoError(t, shipping.PublishDirect(ctx, eventbus.Event{
			EventType: eventType, TenantID: "shop-a", Actor: eventbus.Actor{Type: "shop_admin", ID: "admin"}, Payload: payload,
		}))
	}
	get := func(id uuid.UUID) api.Order {
		t.Helper()
		var o api.Order
		code, _ := admin.do("GET", "/admin/orders/"+id.String(), nil, &o)
		require.Equal(t, http.StatusOK, code)
		return o
	}
	waitFor := func(id uuid.UUID, status string) api.Order {
		t.Helper()
		require.Eventually(t, func() bool { return string(get(id).Status) == status }, 10*time.Second, 50*time.Millisecond)
		return get(id)
	}
	placeAndPackage := func() api.Order {
		t.Helper()
		o := create(newOrder(customerID, "customer"))
		code, _ := confirm(o.OrderID)
		require.Equal(t, http.StatusOK, code)
		var packaged api.Order
		code, _ = admin.do("POST", "/admin/orders/"+o.OrderID.String()+"/package", nil, &packaged)
		require.Equal(t, http.StatusOK, code)
		return packaged
	}

	var parcel api.Order
	t.Run("packaging books the shipment and shipping events move the order on", func(t *testing.T) {
		parcel = placeAndPackage()
		assert.Equal(t, api.OrderStatus("packaged"), parcel.Status)
		require.NotNil(t, parcel.Shipment)
		pay.mu.Lock()
		sh := pay.shipments[parcel.OrderID.String()]
		booked := len(pay.booked)
		req := pay.booked[booked-1]
		pay.mu.Unlock()
		assert.Equal(t, sh["tracking_number"], parcel.Shipment.TrackingNumber)
		assert.Equal(t, "standard", req["method"])
		assert.Equal(t, rateID.String(), req["rate_id"])
		assert.Equal(t, "Cape Town", req["delivery_address"].(map[string]any)["city"])

		var again api.Order
		code, _ := admin.do("POST", "/admin/orders/"+parcel.OrderID.String()+"/package", nil, &again)
		require.Equal(t, http.StatusOK, code, "packaging again")
		pay.mu.Lock()
		assert.Len(t, pay.booked, booked, "booked once")
		pay.mu.Unlock()
		packaged := outbox(events.TypeOrderPackagedV1)
		require.Len(t, packaged, 1)
		assert.Equal(t, parcel.Shipment.TrackingNumber, packaged[0]["tracking_number"])

		digital := newOrder(customerID, "customer")
		digital.Items = digital.Items[1:]
		digital.RequiresShipping, digital.DeliveryAddress, digital.ShippingOption = false, nil, nil
		digital.ItemsTotalMinor, digital.ShippingMinor, digital.TaxTotalMinor, digital.TotalMinor = 500, 0, 65, 500
		d := create(digital)
		code, _ = confirm(d.OrderID)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, http.StatusConflict, admin.code("POST", "/admin/orders/"+d.OrderID.String()+"/package", nil),
			"a digital order skips shipping")

		shipmentID, tracking := parcel.Shipment.ShipmentID.String(), parcel.Shipment.TrackingNumber
		carrier, ref := "The Courier Guy", "TCG-1"
		publish(events.TypeShippingInTransitV1, events.ShippingInTransitV1{
			ShipmentID: shipmentID, OrderID: parcel.OrderID.String(), TrackingNumber: tracking,
			CarrierName: &carrier, CarrierReference: &ref, OccurredAt: time.Now().UTC(),
		})
		o := waitFor(parcel.OrderID, "shipped")
		assert.Equal(t, carrier, *o.Shipment.CarrierName)
		assert.NotNil(t, o.Shipment.ShippedAt)
		assert.Len(t, outbox(events.TypeOrderShippedV1), 1)
		assert.Equal(t, http.StatusConflict, admin.code("POST", "/admin/orders/"+parcel.OrderID.String()+"/cancel", nil),
			"a shipped order is not cancelled")

		publish(events.TypeShippingDeliveredV1, events.ShippingDeliveredV1{
			ShipmentID: shipmentID, OrderID: parcel.OrderID.String(), TrackingNumber: tracking, OccurredAt: time.Now().UTC(),
		})
		o = waitFor(parcel.OrderID, "delivered")
		assert.NotNil(t, o.Shipment.DeliveredAt)
		delivered := outbox(events.TypeOrderDeliveredV1)
		require.Len(t, delivered, 1)
		assert.Len(t, delivered[0]["items"], 2)

		var mine api.Order
		code, _ = customer.do("GET", "/orders/"+parcel.OrderID.String(), nil, &mine)
		require.Equal(t, http.StatusOK, code)
		path := make([]string, 0, len(*mine.History))
		for _, h := range *mine.History {
			path = append(path, string(h.ToStatus))
		}
		assert.Equal(t, []string{"pending_payment", "received", "packaged", "shipped", "delivered"}, path)
	})

	t.Run("customers return delivered items and admins decide", func(t *testing.T) {
		path := "/orders/" + parcel.OrderID.String() + "/returns"
		one := api.ReturnInput{Items: []api.ReturnItem{{SkuID: mug, Quantity: 1}}}
		assert.Equal(t, http.StatusBadRequest, customer.code("POST", path, api.ReturnInput{Items: []api.ReturnItem{{SkuID: ebook, Quantity: 1}}}),
			"digital items are not returned")
		assert.Equal(t, http.StatusNotFound, as(t, srv, "shop-a", uuid.NewString(), "customer").code("POST", path, one),
			"only the customer's own order")
		var r api.Return
		code, _ := customer.do("POST", path, one, &r)
		require.Equal(t, http.StatusCreated, code)
		assert.Equal(t, api.ReturnStatus("requested"), r.Status)
		assert.Equal(t, http.StatusConflict, customer.code("POST", path, one), "one request at a time")
		assert.Len(t, outbox(events.TypeOrderReturnRequestedV1), 1)

		var list api.ReturnList
		code, _ = admin.do("GET", "/admin/returns?status=requested", nil, &list)
		require.Equal(t, http.StatusOK, code)
		require.Len(t, list.Items, 1)

		reject := "/admin/returns/" + r.ReturnID.String() + "/reject"
		assert.Equal(t, http.StatusBadRequest, admin.code("POST", reject, api.RejectReturnInput{}))
		code, _ = admin.do("POST", reject, api.RejectReturnInput{Reason: "Used"}, &r)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, api.ReturnStatus("rejected"), r.Status)
		assert.Len(t, outbox(events.TypeOrderReturnRejectedV1), 1)
		assert.Equal(t, http.StatusConflict, admin.code("POST", "/admin/returns/"+r.ReturnID.String()+"/accept", nil),
			"a rejected return stays rejected")

		code, _ = customer.do("POST", path, api.ReturnInput{Items: []api.ReturnItem{{SkuID: mug, Quantity: 2}}}, &r)
		require.Equal(t, http.StatusCreated, code)
		refundShipping := true
		code, _ = admin.do("POST", "/admin/returns/"+r.ReturnID.String()+"/accept",
			api.AcceptReturnInput{RefundShipping: &refundShipping}, &r)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, api.ReturnStatus("accepted"), r.Status)
		assert.EqualValues(t, 11000, r.Refund, "both mugs and shipping")
		code, _ = admin.do("POST", "/admin/returns/"+r.ReturnID.String()+"/accept", nil, &r)
		require.Equal(t, http.StatusOK, code, "accepting again")
		pay.mu.Lock()
		assert.EqualValues(t, 11000, pay.keys["order:"+parcel.OrderID.String()+":return:"+r.ReturnID.String()])
		pay.mu.Unlock()

		o := get(parcel.OrderID)
		assert.Equal(t, api.OrderStatus("returned"), o.Status)
		assert.EqualValues(t, 11000, o.Refunded.AmountMinor)
		assert.Len(t, *o.Returns, 2)
		require.Len(t, *o.Refunds, 1)
		accepted := outbox(events.TypeOrderReturnAcceptedV1)
		require.Len(t, accepted, 1)
		refunded := outbox(events.TypeOrderRefundedV1)
		assert.Equal(t, r.ReturnID.String(), refunded[len(refunded)-1]["return_id"])
	})

	t.Run("admins refund part of an order once per key", func(t *testing.T) {
		path := "/admin/orders/" + parcel.OrderID.String() + "/refund"
		assert.Equal(t, http.StatusBadRequest, admin.code("POST", path, api.RefundInput{AmountMinor: 100, Reason: "x"}),
			"an Idempotency-Key is required")
		assert.Equal(t, http.StatusBadRequest, admin.code("POST", path, api.RefundInput{AmountMinor: 501, Reason: "x"},
			"Idempotency-Key", uuid.NewString()), "only 500 is left")
		var o api.Order
		key := uuid.NewString()
		code, _ := admin.do("POST", path, api.RefundInput{AmountMinor: 300, Reason: "late delivery"}, &o, "Idempotency-Key", key)
		require.Equal(t, http.StatusOK, code)
		assert.EqualValues(t, 11300, o.Refunded.AmountMinor)
		code, _ = admin.do("POST", path, api.RefundInput{AmountMinor: 300, Reason: "late delivery"}, &o, "Idempotency-Key", key)
		require.Equal(t, http.StatusOK, code)
		assert.EqualValues(t, 11300, get(parcel.OrderID).Refunded.AmountMinor, "refunded once")
		assert.Equal(t, http.StatusBadRequest, admin.code("POST", path, api.RefundInput{AmountMinor: 300, Reason: "again"},
			"Idempotency-Key", uuid.NewString()), "only 200 is left")
	})

	t.Run("a packaged order can be cancelled; exceptions recover", func(t *testing.T) {
		o := placeAndPackage()
		var cancelled api.Order
		code, _ := admin.do("POST", "/admin/orders/"+o.OrderID.String()+"/cancel", nil, &cancelled)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, api.OrderStatus("cancelled"), cancelled.Status)

		o = placeAndPackage()
		shipmentID, orderID, tracking := o.Shipment.ShipmentID.String(), o.OrderID.String(), o.Shipment.TrackingNumber
		publish(events.TypeShippingDeliveryExceptionV1, events.ShippingDeliveryExceptionV1{
			ShipmentID: shipmentID, OrderID: orderID, TrackingNumber: tracking, Message: "Nobody home", OccurredAt: time.Now().UTC(),
		})
		waitFor(o.OrderID, "delivery_exception")
		publish(events.TypeShippingDeliveredV1, events.ShippingDeliveredV1{
			ShipmentID: shipmentID, OrderID: orderID, TrackingNumber: tracking, OccurredAt: time.Now().UTC(),
		})
		waitFor(o.OrderID, "delivered")

		publish(events.TypeShippingInTransitV1, events.ShippingInTransitV1{
			ShipmentID: uuid.NewString(), OrderID: orderID, TrackingNumber: "YC000000000000", OccurredAt: time.Now().UTC(),
		})
		var mine api.OrderList
		code, _ = customer.do("GET", "/orders/mine?page_size=100", nil, &mine)
		require.Equal(t, http.StatusOK, code)
		statuses := map[string]bool{}
		for _, it := range mine.Items {
			statuses[string(it.Status)] = true
		}
		assert.True(t, statuses["delivered"] && statuses["returned"] && statuses["cancelled"], statuses)
	})

	t.Run("a chargeback disputes the order once", func(t *testing.T) {
		o := create(newOrder(customerID, "customer"))
		code, _ := confirm(o.OrderID)
		require.Equal(t, http.StatusOK, code)
		orderID, reason := o.OrderID.String(), "fraudulent"
		cb := events.PaymentChargebackReceivedV1{
			ChargebackID: uuid.NewString(), TransactionID: tx.String(), OrderID: &orderID, Gateway: "stripe",
			GatewayReference: "dp_1", Amount: events.Money{AmountMinor: 11500, Currency: "ZAR"}, Reason: &reason,
			ReceivedAt: time.Now().UTC(),
		}
		publish(events.TypePaymentChargebackReceivedV1, cb)
		publish(events.TypePaymentChargebackReceivedV1, cb)
		disputed := waitFor(o.OrderID, "disputed")
		assert.Equal(t, api.OrderStatus("received"), *(*disputed.History)[len(*disputed.History)-1].FromStatus)
		require.Eventually(t, func() bool { return len(outbox(events.TypeOrderDisputedV1)) == 1 }, 10*time.Second, 50*time.Millisecond)
		time.Sleep(200 * time.Millisecond)
		got := outbox(events.TypeOrderDisputedV1)
		require.Len(t, got, 1, "a repeated chargeback changes nothing")
		assert.Equal(t, cb.ChargebackID, got[0]["chargeback_id"])
		assert.Equal(t, customerID, got[0]["user_id"])
	})

	t.Run("a deleted customer's orders are pseudonymised", func(t *testing.T) {
		userID := uuid.NewString()
		lookup := func(ids ...string) api.CustomerOrders {
			t.Helper()
			q := url.Values{"user_id": ids}
			var out api.CustomerOrders
			code, _ := svc.do("GET", "/internal/customer-orders?"+q.Encode(), nil, &out)
			require.Equal(t, http.StatusOK, code)
			return out
		}
		o := create(newOrder(userID, "customer"))
		assert.Equal(t, 1, lookup(userID).OpenCount, "a pending order is unfinished")
		code, _ := confirm(o.OrderID)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, 1, lookup(userID).OpenCount)
		require.Equal(t, http.StatusOK, admin.code("POST", "/admin/orders/"+o.OrderID.String()+"/cancel", nil))
		assert.Equal(t, 0, lookup(userID).OpenCount)
		assert.Len(t, lookup(userID).Orders, 1)
		assert.Equal(t, http.StatusBadRequest, svc.code("GET", "/internal/customer-orders", nil))

		pseudonym := uuid.NewString()
		deleted := events.IdentityUserDeletedV1{
			UserID: userID, Pseudonym: pseudonym, Role: "customer", RequestedBy: "user", DeletedAt: time.Now().UTC(),
		}
		publish(events.TypeIdentityUserDeletedV1, deleted)
		publish(events.TypeIdentityUserDeletedV1, deleted)
		completed := func() []map[string]any {
			var out []map[string]any
			for _, p := range outbox(events.TypePlatformGdprCascadeCompletedV1) {
				if p["user_id"] == userID {
					out = append(out, p)
				}
			}
			return out
		}
		require.Eventually(t, func() bool { return len(completed()) == 1 }, 10*time.Second, 50*time.Millisecond)
		time.Sleep(200 * time.Millisecond)
		done := completed()
		require.Len(t, done, 1, "a repeated deletion changes nothing")
		assert.Equal(t, "yc-orders", done[0]["service"])
		assert.Equal(t, "pseudonymized", done[0]["action"])
		assert.EqualValues(t, 1, done[0]["record_count"])

		got := get(o.OrderID)
		assert.Equal(t, pseudonym, got.UserID)
		assert.Nil(t, got.DeliveryAddress)
		assert.EqualValues(t, 11500, got.Total.AmountMinor, "amounts stay for the books")
		assert.Empty(t, lookup(userID).Orders)
		assert.Len(t, lookup(userID, pseudonym).Orders, 1)

		late := create(newOrder(userID, "customer"))
		code, placed := confirm(late.OrderID)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, pseudonym, placed.UserID, "an order paid for during the deletion is pseudonymised too")
		assert.Nil(t, placed.DeliveryAddress)
	})
}
