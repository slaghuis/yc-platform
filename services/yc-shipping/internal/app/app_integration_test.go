//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
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
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	testkit "github.com/slaghuis/yc-platform/libs/yc-testkit"
	api "github.com/slaghuis/yc-platform/services/yc-shipping/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-shipping/internal/app"
)

type client struct {
	t   *testing.T
	srv *httptest.Server
	hdr map[string]string
}

// as returns a client sending the headers yc-gateway would set.
func as(t *testing.T, srv *httptest.Server, tenant, actorID, actorType string) client {
	return client{t: t, srv: srv, hdr: map[string]string{
		authctx.HeaderTenantID:  tenant,
		authctx.HeaderActorID:   actorID,
		authctx.HeaderActorType: actorType,
	}}
}

// service is another service calling through the gateway's internal
// listener.
func service(t *testing.T, srv *httptest.Server, tenant, name string) client {
	c := as(t, srv, tenant, name, "service")
	c.hdr[authctx.HeaderCallerService] = name
	return c
}

func (c client) do(method, path string, body, out any, header ...string) int {
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
	if out != nil && resp.StatusCode < 300 {
		require.NoError(c.t, json.NewDecoder(resp.Body).Decode(out))
	}
	return resp.StatusCode
}

func address(province string) map[string]any {
	return map[string]any{
		"country_code": "ZA", "recipient": "Thandi", "lines": []string{"1 Main Rd"},
		"fields": map[string]any{"province": province, "city": "Somewhere", "postal_code": "0001"},
	}
}

func TestShipping(t *testing.T) {
	ctx := context.Background()
	logs := telemetry.InitForTest(t, "yc-shipping")
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	})

	dbURL := testkit.Postgres(t)
	natsURL := testkit.NATS(t)
	t.Setenv("YC_SHIPPING_DB_URL", dbURL)
	t.Setenv("NATS_URL", natsURL)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	// yc-gateway in front of yc-orders: customerOrders maps a user ID to its
	// orders.
	var (
		mu             sync.Mutex
		customerOrders = map[string][]string{}
	)
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/internal/service-token":
			_, _ = w.Write([]byte(`{"access_token":"svc-yc-shipping","expires_at":"2099-01-01T00:00:00Z"}`))
		case "/internal/customer-orders":
			if r.Header.Get(authctx.HeaderAuthorization) != "Bearer svc-yc-shipping" || r.Header.Get(authctx.HeaderTenantID) != "shop-a" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			out := []map[string]string{}
			for _, u := range r.URL.Query()["user_id"] {
				for _, id := range customerOrders[u] {
					out = append(out, map[string]string{"order_id": id, "status": "delivered"})
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"orders": out, "open_count": 0})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(gw.Close)
	t.Setenv("YC_GATEWAY_INTERNAL_URL", gw.URL)
	t.Setenv("YC_SHIPPING_SERVICE_SECRET", "shipping-secret")
	cfg, err := app.LoadConfig()
	require.NoError(t, err)
	cfg.Gateway.InternalURL = ""
	require.NoError(t, cfg.Validate())
	a, err := app.New(ctx, cfg, httpkit.BuildInfo{Service: "yc-shipping", Version: "test"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	srv := httptest.NewServer(a.Handler)
	t.Cleanup(srv.Close)

	db, err := pgxpool.New(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(db.Close)
	outbox := func(tenant, eventType string) []map[string]any {
		rows, err := db.Query(ctx, `SELECT envelope->'payload' FROM outbox
			WHERE tenant_id = $1 AND envelope->>'event_type' = $2 ORDER BY created_at, id`, tenant, eventType)
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

	validator, err := events.NewValidator()
	require.NoError(t, err)
	upstream, err := eventbus.NewClient(ctx, eventbus.Config{
		NATSURL: natsURL, ServiceName: "yc-orders", Validator: validator,
		Streams: []eventbus.StreamConfig{eventbus.OrderStream()},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upstream.Close(context.Background()) })

	admin := as(t, srv, "shop-a", uuid.NewString(), "shop_admin")
	customer := as(t, srv, "shop-a", uuid.NewString(), "customer")
	guest := as(t, srv, "shop-a", "", "visitor")
	checkout := service(t, srv, "shop-a", "yc-checkout")
	orders := service(t, srv, "shop-a", "yc-orders")

	zoneIDs := map[string]string{}
	t.Run("admins define zones", func(t *testing.T) {
		for _, z := range []api.ZoneInput{
			{Name: "Gauteng", Priority: ptr(10), MatchRules: []api.MatchRule{{CountryCode: "ZA", Fields: &map[string]string{"province": "Gauteng"}}}},
			{Name: "Western Cape", Priority: ptr(10), MatchRules: []api.MatchRule{{CountryCode: "za", Fields: &map[string]string{"province": "Western Cape"}}}},
			{Name: "Rest of SA", MatchRules: []api.MatchRule{{CountryCode: "ZA"}}},
		} {
			var out api.Zone
			require.Equal(t, http.StatusCreated, admin.do("POST", "/admin/zones", z, &out))
			zoneIDs[z.Name] = out.ZoneID.String()
		}
		assert.Equal(t, http.StatusForbidden, customer.do("POST", "/admin/zones", api.ZoneInput{Name: "x"}, nil))
		assert.Equal(t, http.StatusBadRequest, admin.do("POST", "/admin/zones", api.ZoneInput{
			Name:       "x",
			MatchRules: []api.MatchRule{{CountryCode: "ZAF"}},
		}, nil))

		var list api.ZoneList
		require.Equal(t, http.StatusOK, admin.do("GET", "/admin/zones", nil, &list))
		require.Len(t, list.Items, 3)
		assert.Equal(t, "Rest of SA", list.Items[2].Name, "lowest priority last")
		assert.Equal(t, 100, list.Items[2].Priority, "default priority")
		assert.Equal(t, "ZA", list.Items[1].MatchRules[0].CountryCode)

		var public api.PublicZoneList
		require.Equal(t, http.StatusOK, guest.do("GET", "/shipping/zones", nil, &public))
		assert.Len(t, public.Items, 3)
	})

	rateIDs := map[string]string{}
	t.Run("admins price each zone", func(t *testing.T) {
		create := func(zone, method string, in api.RateInput) {
			t.Helper()
			in.ZoneID, in.Method = uuid.MustParse(zoneIDs[zone]), method
			var out api.Rate
			require.Equal(t, http.StatusCreated, admin.do("POST", "/admin/rates", in, &out))
			assert.Equal(t, "manual", out.CarrierDriver)
			assert.True(t, out.Active)
			rateIDs[zone+"/"+method] = out.RateID.String()
		}
		create("Gauteng", "standard", api.RateInput{
			DisplayName: "Standard", PricingType: api.Flat,
			FlatAmountMinor: ptr[int64](6000), EstimatedDaysMin: ptr(1), EstimatedDaysMax: ptr(2),
		})
		create("Gauteng", "express", api.RateInput{
			DisplayName: "Express", PricingType: api.WeightTiered,
			WeightTiers: &[]api.WeightTier{{MaxWeightG: 2000, AmountMinor: 12000}, {MaxWeightG: 10000, AmountMinor: 18000}},
		})
		create("Western Cape", "standard", api.RateInput{DisplayName: "Standard", PricingType: api.Flat, FlatAmountMinor: ptr[int64](8000)})
		create("Rest of SA", "standard", api.RateInput{
			DisplayName: "Standard", PricingType: api.PriceTiered,
			PriceTiers: &[]api.PriceTier{{MinSubtotalMinor: 0, AmountMinor: 9500}, {MinSubtotalMinor: 100000, AmountMinor: 0}},
		})

		dup := api.RateInput{ZoneID: uuid.MustParse(zoneIDs["Gauteng"]), Method: "standard", DisplayName: "Again", PricingType: api.Flat}
		assert.Equal(t, http.StatusConflict, admin.do("POST", "/admin/rates", dup, nil))
		dup.ZoneID = uuid.New()
		assert.Equal(t, http.StatusBadRequest, admin.do("POST", "/admin/rates", dup, nil), "no such zone")

		var list api.RateList
		require.Equal(t, http.StatusOK, admin.do("GET", "/admin/rates?zone_id="+zoneIDs["Gauteng"], nil, &list))
		assert.Len(t, list.Items, 2)
	})

	quote := func(addr map[string]any, subtotal int64, items ...api.QuoteItem) api.QuoteResult {
		t.Helper()
		var q api.QuoteResult
		require.Equal(t, http.StatusOK, checkout.do("POST", "/internal/quote",
			api.QuoteInput{DeliveryAddress: addr, Items: items, SubtotalMinor: subtotal}, &q))
		return q
	}
	book := api.QuoteItem{SkuID: uuid.New(), Quantity: 2, WeightGrams: ptr[int64](600)}

	t.Run("the address picks the zone and its options", func(t *testing.T) {
		q := quote(address("gauteng"), 30000, book)
		require.NotNil(t, q.Zone)
		assert.Equal(t, "Gauteng", q.Zone.Name)
		require.Len(t, q.Options, 2)
		assert.Equal(t, "standard", q.Options[0].OptionID, "cheapest first")
		assert.Equal(t, int64(6000), q.Options[0].AmountMinor)
		assert.Equal(t, int64(12000), q.Options[1].AmountMinor, "1.2 kg is the first tier")

		unweighed := api.QuoteItem{SkuID: uuid.New(), Quantity: 1}
		q = quote(address("Gauteng"), 30000, book, unweighed)
		assert.Len(t, q.Options, 1, "no weight-based rate without every weight")
		q = quote(address("Gauteng"), 30000, book, api.QuoteItem{SkuID: uuid.New(), Quantity: 1, IsDigital: ptr(true)})
		assert.Len(t, q.Options, 2, "digital lines weigh nothing")

		assert.Equal(t, int64(8000), quote(address("Western Cape"), 30000, book).Options[0].AmountMinor)
		assert.Equal(t, int64(9500), quote(address("Limpopo"), 30000, book).Options[0].AmountMinor)
		assert.Equal(t, int64(0), quote(address("Limpopo"), 100000, book).Options[0].AmountMinor, "free over R1000")

		abroad := quote(map[string]any{"country_code": "NA", "lines": []string{"x"}}, 30000, book)
		assert.Nil(t, abroad.Zone)
		assert.Empty(t, abroad.Options, "the shop does not ship there")
	})

	t.Run("a rate turned off is not offered", func(t *testing.T) {
		in := api.RateInput{
			ZoneID: uuid.MustParse(zoneIDs["Gauteng"]), Method: "express", DisplayName: "Express",
			PricingType: api.Flat, FlatAmountMinor: ptr[int64](15000), Active: ptr(false),
		}
		require.Equal(t, http.StatusOK, admin.do("PUT", "/admin/rates/"+rateIDs["Gauteng/express"], in, nil))
		assert.Len(t, quote(address("Gauteng"), 30000, book).Options, 1)
		in.Active = ptr(true)
		require.Equal(t, http.StatusOK, admin.do("PUT", "/admin/rates/"+rateIDs["Gauteng/express"], in, nil))
	})

	orderID := uuid.New()
	var sh api.Shipment
	t.Run("yc-orders books a shipment once", func(t *testing.T) {
		in := api.ShipmentInput{
			OrderID: orderID, OrderNumber: "YC-1001", Method: "standard",
			RateID: ptr(uuid.MustParse(rateIDs["Gauteng/standard"])), DeliveryAddress: address("Gauteng"),
		}
		assert.Equal(t, http.StatusBadRequest, orders.do("POST", "/internal/shipments", in, nil), "an Idempotency-Key is required")
		require.Equal(t, http.StatusOK, orders.do("POST", "/internal/shipments", in, &sh, "Idempotency-Key", "order:1:package"))
		assert.Equal(t, api.ShipmentStatus("label_created"), sh.Status)
		assert.Regexp(t, `^YC[0-9A-Z]{12}$`, sh.TrackingNumber)
		assert.Equal(t, "manual", sh.CarrierDriver)

		var again api.Shipment
		require.Equal(t, http.StatusOK, orders.do("POST", "/internal/shipments", in, &again, "Idempotency-Key", "order:1:package-retry"))
		assert.Equal(t, sh.ShipmentID, again.ShipmentID, "one shipment per order")

		created := outbox("shop-a", events.TypeShippingLabelCreatedV1)
		require.Len(t, created, 1)
		assert.Equal(t, sh.TrackingNumber, created[0]["tracking_number"])
		assert.Equal(t, orderID.String(), created[0]["order_id"])
	})

	t.Run("admins report progress", func(t *testing.T) {
		path := "/admin/shipments/" + sh.ShipmentID.String() + "/manual-update"
		assert.Equal(t, http.StatusConflict, admin.do("POST", path, api.ManualUpdateInput{Status: "delivered"}, nil),
			"not delivered before it left")
		assert.Equal(t, http.StatusBadRequest, admin.do("POST", path, api.ManualUpdateInput{Status: "exception"}, nil),
			"an exception needs a message")

		var d api.ShipmentDetail
		require.Equal(t, http.StatusOK, admin.do("POST", path, api.ManualUpdateInput{
			Status:      "in_transit",
			CarrierName: ptr("The Courier Guy"), CarrierReference: ptr("TCG-77"),
		}, &d))
		assert.Equal(t, api.ShipmentStatus("in_transit"), d.Status)
		assert.Equal(t, "The Courier Guy", *d.CarrierName)
		require.Len(t, d.Events, 2)

		transit := outbox("shop-a", events.TypeShippingInTransitV1)
		require.Len(t, transit, 1)
		assert.Equal(t, "TCG-77", transit[0]["carrier_reference"])

		require.Equal(t, http.StatusOK, admin.do("POST", path, api.ManualUpdateInput{Status: "delivered", Message: ptr("Left with security")}, &d))
		assert.Len(t, outbox("shop-a", events.TypeShippingDeliveredV1), 1)
		assert.Equal(t, http.StatusConflict, admin.do("POST", path, api.ManualUpdateInput{Status: "in_transit"}, nil), "delivered is final")

		var list api.ShipmentList
		require.Equal(t, http.StatusOK, admin.do("GET", "/admin/shipments?status=delivered", nil, &list))
		require.Len(t, list.Items, 1)
		assert.Equal(t, sh.ShipmentID, list.Items[0].ShipmentID)
	})

	t.Run("anyone with the tracking number sees progress", func(t *testing.T) {
		var tr api.Tracking
		require.Equal(t, http.StatusOK, guest.do("GET", "/shipping/tracking/"+sh.TrackingNumber, nil, &tr))
		assert.Equal(t, api.ShipmentStatus("delivered"), tr.Status)
		assert.Len(t, tr.Events, 3)
		raw := map[string]any{}
		guest.do("GET", "/shipping/tracking/"+sh.TrackingNumber, nil, &raw)
		assert.NotContains(t, raw, "delivery_address")
		assert.NotContains(t, raw, "order_id")
		assert.Equal(t, http.StatusNotFound, guest.do("GET", "/shipping/tracking/YC0000000000000", nil, nil))
		other := as(t, srv, "shop-b", "", "visitor")
		assert.Equal(t, http.StatusNotFound, other.do("GET", "/shipping/tracking/"+sh.TrackingNumber, nil, nil),
			"tracking is per shop")
	})

	t.Run("a cancelled order's shipment is cancelled", func(t *testing.T) {
		cancelled := uuid.New()
		var s2 api.Shipment
		require.Equal(t, http.StatusOK, orders.do("POST", "/internal/shipments", api.ShipmentInput{
			OrderID: cancelled, OrderNumber: "YC-1002", Method: "standard", DeliveryAddress: address("Limpopo"),
		}, &s2, "Idempotency-Key", "order:2:package"))
		require.NoError(t, upstream.PublishDirect(ctx, eventbus.Event{
			EventType: events.TypeOrderCancelledV1, TenantID: "shop-a",
			Actor: eventbus.Actor{Type: "service", ID: "yc-orders"},
			Payload: events.OrderCancelledV1{
				OrderID: cancelled.String(), OrderNumber: "YC-1002",
				UserID: uuid.NewString(), Reason: "customer changed their mind", CancelledAt: time.Now().UTC(),
			},
		}))
		require.Eventually(t, func() bool { return len(outbox("shop-a", events.TypeShippingCancelledV1)) == 1 },
			10*time.Second, 50*time.Millisecond)
		var d api.ShipmentDetail
		require.Equal(t, http.StatusOK, admin.do("GET", "/admin/shipments/"+s2.ShipmentID.String(), nil, &d))
		assert.Equal(t, api.ShipmentStatus("cancelled"), d.Status)
	})

	t.Run("a deleted customer's addresses are cleared (GDPR)", func(t *testing.T) {
		userID, pseudonym := uuid.NewString(), uuid.NewString()
		mu.Lock()
		customerOrders[pseudonym] = []string{orderID.String(), uuid.NewString()}
		mu.Unlock()
		require.NoError(t, upstream.PublishDirect(ctx, eventbus.Event{
			EventType: events.TypeIdentityUserDeletedV1, TenantID: "shop-a",
			Actor: eventbus.Actor{Type: "service", ID: "yc-identity"},
			Payload: events.IdentityUserDeletedV1{
				UserID: userID, Pseudonym: pseudonym, Role: "customer", RequestedBy: "user", DeletedAt: time.Now().UTC(),
			},
		}))
		require.Eventually(t, func() bool { return len(outbox("shop-a", events.TypePlatformGdprCascadeCompletedV1)) == 1 },
			10*time.Second, 50*time.Millisecond)
		done := outbox("shop-a", events.TypePlatformGdprCascadeCompletedV1)[0]
		assert.Equal(t, "yc-shipping", done["service"])
		assert.Equal(t, "anonymized", done["action"])
		assert.EqualValues(t, 1, done["record_count"], "found by the pseudonym; the other order has no shipment")

		var d api.ShipmentDetail
		require.Equal(t, http.StatusOK, admin.do("GET", "/admin/shipments/"+sh.ShipmentID.String(), nil, &d))
		assert.Empty(t, d.DeliveryAddress)
		assert.Equal(t, sh.TrackingNumber, d.TrackingNumber, "the parcel's record stays")
	})

	t.Run("deleting a zone removes its rates", func(t *testing.T) {
		require.Equal(t, http.StatusNoContent, admin.do("DELETE", "/admin/zones/"+zoneIDs["Western Cape"], nil, nil))
		assert.Equal(t, int64(9500), quote(address("Western Cape"), 30000, book).Options[0].AmountMinor,
			"falls through to the rest of SA")
		var list api.RateList
		require.Equal(t, http.StatusOK, admin.do("GET", "/admin/rates", nil, &list))
		assert.Len(t, list.Items, 3)
	})
}

func ptr[T any](v T) *T { return &v }
