//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
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
// yc-payment's refunds.
type fakePayment struct {
	mu      sync.Mutex
	keys    map[string]int64 // Idempotency-Key → amount refunded
	calls   int
	refuses bool
}

func (f *fakePayment) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/internal/service-token":
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "svc-yc-orders", "expires_at": time.Now().Add(time.Hour)})
	case "/internal/refund":
		if r.Header.Get("Authorization") != "Bearer svc-yc-orders" || r.Header.Get(authctx.HeaderTenantID) == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
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
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "completed", "refund": map[string]any{"amount_minor": f.keys[key]},
		})
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

	pay := &fakePayment{keys: map[string]int64{}}
	gw := httptest.NewServer(pay)
	t.Cleanup(gw.Close)
	dbURL := testkit.Postgres(t)
	t.Setenv("YC_ORDERS_DB_URL", dbURL)
	t.Setenv("NATS_URL", testkit.NATS(t))
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
	mug, ebook := uuid.New(), uuid.New()
	tx := uuid.New()
	newOrder := func(userID, userType string) api.CreateOrderInput {
		digital := true
		code := "MUG-1"
		return api.CreateOrderInput{
			CheckoutSessionID: uuid.New(), UserID: userID, UserType: api.CreateOrderInputUserType(userType),
			RequiresShipping: true, DeliveryAddress: &map[string]any{"line1": "1 Long St", "city": "Cape Town"},
			Items: []api.OrderItemInput{
				{SkuID: mug, SkuCode: &code, NameSnapshot: "Mug", Quantity: 2, UnitPriceMinor: 5000, LineTotalMinor: 10000},
				{SkuID: ebook, NameSnapshot: "E-book", Quantity: 1, UnitPriceMinor: 500, LineTotalMinor: 500, IsDigital: &digital},
			},
			Currency: "ZAR", ItemsTotalMinor: 10500, ShippingMinor: 1000, TaxTotalMinor: 1370, TotalMinor: 11500,
			PricingSnapshot: map[string]any{"quote_id": "q1"},
			ShippingOption:  &map[string]any{"code": "standard", "amount_minor": 1000},
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
}
