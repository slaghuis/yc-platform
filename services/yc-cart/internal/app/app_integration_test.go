//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	money "github.com/slaghuis/yc-platform/libs/yc-money"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	testkit "github.com/slaghuis/yc-platform/libs/yc-testkit"
	api "github.com/slaghuis/yc-platform/services/yc-cart/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-cart/internal/app"
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

func (c client) do(method, path string, body, out any) int {
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
	resp, err := c.srv.Client().Do(req)
	require.NoError(c.t, err)
	defer resp.Body.Close()
	if out != nil && resp.StatusCode < 300 {
		require.NoError(c.t, json.NewDecoder(resp.Body).Decode(out))
	}
	return resp.StatusCode
}

func (c client) cart(cartType, query string) api.Cart {
	c.t.Helper()
	var out api.Cart
	require.Equal(c.t, http.StatusOK, c.do("GET", "/carts/"+cartType+query, nil, &out))
	return out
}

// fakePricing stands in for yc-gateway's internal listener in front of
// yc-pricing: ZAR gross prices, and USD at 0.05 per ZAR.
type fakePricing struct {
	mu       sync.Mutex
	prices   map[string]int64 // SKU → gross minor units
	unpriced map[string]bool  // SKUs without a base price
	tenants  []string
}

func (f *fakePricing) set(sku string, minor int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prices[sku] = minor
}

// callers returns the tenants pricing was called for.
func (f *fakePricing) callers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return unique(f.tenants)
}

func (f *fakePricing) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/internal/service-token":
		var in struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.ClientID != "yc-cart" || in.ClientSecret != "cart-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"access_token": "svc-yc-cart", "expires_at": time.Now().Add(time.Hour)})
	case "/internal/prices/evaluate":
		if r.Header.Get("Authorization") != "Bearer svc-yc-cart" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var in struct {
			Lines []struct {
				SKUID    string `json:"sku_id"`
				Quantity int64  `json:"quantity"`
			} `json:"lines"`
			DisplayCurrency string `json:"display_currency"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.DisplayCurrency != "" && in.DisplayCurrency != "USD" && in.DisplayCurrency != "ZAR" {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"type":"about:blank","title":"Bad","status":400,"code":"VALIDATION_FAILED"}`))
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.tenants = append(f.tenants, r.Header.Get(authctx.HeaderTenantID))
		type line struct {
			SKUID string      `json:"sku_id"`
			Gross money.Money `json:"gross"`
		}
		type unpriced struct {
			SKUID  string `json:"sku_id"`
			Reason string `json:"reason"`
		}
		out := struct {
			StoreCurrency string     `json:"store_currency"`
			Lines         []line     `json:"lines"`
			Unpriced      []unpriced `json:"unpriced"`
			Display       *struct {
				Currency      string    `json:"currency"`
				Rate          string    `json:"rate"`
				RateFetchedAt time.Time `json:"rate_fetched_at"`
				Lines         []line    `json:"lines"`
			} `json:"display,omitempty"`
		}{StoreCurrency: "ZAR", Lines: []line{}, Unpriced: []unpriced{}}
		if in.DisplayCurrency == "USD" {
			out.Display = &struct {
				Currency      string    `json:"currency"`
				Rate          string    `json:"rate"`
				RateFetchedAt time.Time `json:"rate_fetched_at"`
				Lines         []line    `json:"lines"`
			}{Currency: "USD", Rate: "0.05", RateFetchedAt: time.Now().UTC()}
		}
		for _, l := range in.Lines {
			qty := l.Quantity
			if qty < 1 {
				qty = 1
			}
			minor, ok := f.prices[l.SKUID]
			switch {
			case ok:
				out.Lines = append(out.Lines, line{l.SKUID, money.New(minor*qty, "ZAR")})
				if out.Display != nil {
					out.Display.Lines = append(out.Display.Lines, line{l.SKUID, money.New((minor/20)*qty, "USD")})
				}
			case f.unpriced[l.SKUID]:
				out.Unpriced = append(out.Unpriced, unpriced{l.SKUID, "no_base_price"})
			default:
				out.Unpriced = append(out.Unpriced, unpriced{l.SKUID, "unknown_sku"})
			}
		}
		writeJSON(w, out)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestCart(t *testing.T) {
	ctx := context.Background()
	logs := telemetry.InitForTest(t, "yc-cart")
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	})

	skuA, skuB, skuC := uuid.NewString(), uuid.NewString(), uuid.NewString()
	noPrice := uuid.NewString()
	pricing := &fakePricing{
		prices:   map[string]int64{skuA: 10000, skuB: 2500, skuC: 400},
		unpriced: map[string]bool{noPrice: true},
	}
	gw := httptest.NewServer(pricing)
	t.Cleanup(gw.Close)

	dbURL := testkit.Postgres(t)
	natsURL := testkit.NATS(t)
	t.Setenv("YC_CART_DB_URL", dbURL)
	t.Setenv("NATS_URL", natsURL)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("YC_GATEWAY_INTERNAL_URL", gw.URL)
	t.Setenv("YC_CART_SERVICE_SECRET", "cart-secret")
	t.Setenv("YC_CART_ABANDONED_AFTER", "1h")
	t.Setenv("YC_CART_ABANDONED_FOLLOW_UP", "24h")
	t.Setenv("YC_CART_ABANDONED_FINAL", "72h")
	t.Setenv("YC_CART_ABANDONED_SWEEP_INTERVAL", "1h")
	cfg, err := app.LoadConfig()
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())
	a, err := app.New(ctx, cfg, httpkit.BuildInfo{Service: "yc-cart", Version: "test"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	srv := httptest.NewServer(a.Handler)
	t.Cleanup(srv.Close)

	db, err := pgxpool.New(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(db.Close)
	testkit.AssertTenantIsolation(t, db, "outbox")
	// outbox returns the payloads of tenant's eventType events, oldest
	// first.
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
		NATSURL: natsURL, ServiceName: "yc-upstream", Validator: validator,
		Streams: []eventbus.StreamConfig{eventbus.IdentityStream(), eventbus.PreferenceStream(), eventbus.ProductStream(), eventbus.CheckoutStream()},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upstream.Close(context.Background()) })
	publish := func(tenant, eventType string, payload any) {
		t.Helper()
		require.NoError(t, upstream.PublishDirect(ctx, eventbus.Event{
			EventType: eventType, TenantID: tenant,
			Actor: eventbus.Actor{Type: "service", ID: "yc-upstream"}, Payload: payload,
		}))
	}

	visitorID := uuid.NewString()
	visitor := as(t, srv, "shop-a", visitorID, "visitor")
	add := func(c client, cartType, sku string, quantity int) (int, api.Cart) {
		t.Helper()
		var out api.Cart
		code := c.do("POST", "/carts/"+cartType+"/items", map[string]any{"sku_id": sku, "quantity": quantity}, &out)
		return code, out
	}

	t.Run("a new visitor has empty carts of every type", func(t *testing.T) {
		var list api.CartList
		require.Equal(t, http.StatusOK, visitor.do("GET", "/carts", nil, &list))
		require.Len(t, list.Items, 2)
		assert.Equal(t, "shopping", list.Items[0].CartType)
		assert.True(t, list.Items[0].Convertible)
		assert.Nil(t, list.Items[0].CartID)
		assert.Equal(t, "wishlist", list.Items[1].CartType)

		c := visitor.cart("shopping", "")
		assert.Empty(t, c.Items)
		assert.Nil(t, c.Subtotal)
		assert.Equal(t, http.StatusNotFound, visitor.do("GET", "/carts/layaway", nil, nil))
	})

	t.Run("a visitor adds items with a price snapshot", func(t *testing.T) {
		code, c := add(visitor, "shopping", skuA, 2)
		require.Equal(t, http.StatusOK, code)
		require.Len(t, c.Items, 1)
		require.NotNil(t, c.CartID)
		assert.Equal(t, money.New(10000, "ZAR"), c.Items[0].UnitPrice)
		assert.Equal(t, money.New(20000, "ZAR"), *c.Items[0].LineTotal)
		assert.Equal(t, money.New(20000, "ZAR"), *c.Subtotal)
		assert.False(t, c.PriceChanged)

		code, c = add(visitor, "shopping", skuA, 1)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, 3, c.Items[0].Quantity, "adding again sums onto the line")
		code, c = add(visitor, "shopping", skuB, 1)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, 4, c.Quantity)
		assert.Equal(t, money.New(32500, "ZAR"), *c.Subtotal)

		created := outbox("shop-a", events.TypeCartCreatedV1)
		require.Len(t, created, 1)
		assert.Equal(t, "visitor", created[0]["user_type"])
		added := outbox("shop-a", events.TypeCartItemAddedV1)
		require.Len(t, added, 3)
		assert.Equal(t, float64(1), added[1]["quantity_added"])
		assert.Equal(t, float64(3), added[1]["quantity"])
		assert.Equal(t, []string{"shop-a"}, pricing.callers(), "pricing is called in the caller's shop")
	})

	t.Run("items that cannot be priced are refused", func(t *testing.T) {
		code, _ := add(visitor, "shopping", uuid.NewString(), 1)
		assert.Equal(t, http.StatusNotFound, code)
		code, _ = add(visitor, "shopping", noPrice, 1)
		assert.Equal(t, http.StatusConflict, code)
		code, _ = add(visitor, "shopping", skuA, 99)
		assert.Equal(t, http.StatusConflict, code, "over the quantity cap")
		code, _ = add(visitor, "shopping", skuA, 0)
		assert.Equal(t, http.StatusBadRequest, code)
	})

	t.Run("a price change is flagged on read", func(t *testing.T) {
		pricing.set(skuB, 3000)
		t.Cleanup(func() { pricing.set(skuB, 2500) })
		c := visitor.cart("shopping", "")
		assert.True(t, c.PriceChanged)
		require.Len(t, c.Items, 2)
		b := c.Items[1]
		assert.True(t, b.PriceChanged)
		assert.Equal(t, money.New(2500, "ZAR"), b.UnitPrice, "the snapshot is kept")
		assert.Equal(t, money.New(3000, "ZAR"), *b.CurrentUnitPrice)
		assert.Equal(t, money.New(33000, "ZAR"), *c.Subtotal, "totals use current prices")
	})

	t.Run("totals are converted to the display currency", func(t *testing.T) {
		c := visitor.cart("shopping", "?display_currency=USD")
		require.NotNil(t, c.Display)
		assert.Equal(t, "USD", c.Display.Currency)
		assert.Equal(t, money.New(1625, "USD"), c.Display.Subtotal)
		assert.Equal(t, money.New(1500, "USD"), c.Items[0].Display.LineTotal)

		c = visitor.cart("shopping", "?display_currency=EUR")
		assert.Nil(t, c.Display, "without a rate the cart is shown in the store currency")
		assert.Equal(t, money.New(32500, "ZAR"), *c.Subtotal)
	})

	t.Run("quantities change and items move between carts", func(t *testing.T) {
		var c api.Cart
		require.Equal(t, http.StatusOK, visitor.do("PUT", "/carts/shopping/items/"+skuB, map[string]int{"quantity": 4}, &c))
		assert.Equal(t, 4, c.Items[1].Quantity)
		changed := outbox("shop-a", events.TypeCartItemQuantityChangedV1)
		require.Len(t, changed, 1)
		assert.Equal(t, float64(1), changed[0]["previous_quantity"])

		require.Equal(t, http.StatusOK,
			visitor.do("POST", "/carts/shopping/items/"+skuB+"/move", map[string]any{"to_cart_type": "wishlist", "quantity": 3}, &c))
		assert.Equal(t, "wishlist", c.CartType)
		require.Len(t, c.Items, 1)
		assert.Equal(t, 3, c.Items[0].Quantity)
		assert.Equal(t, money.New(2500, "ZAR"), c.Items[0].UnitPrice, "the snapshot moves with the line")
		assert.Equal(t, 1, visitor.cart("shopping", "").Items[1].Quantity)

		assert.Equal(t, http.StatusNotFound, visitor.do("PUT", "/carts/shopping/items/"+skuC, map[string]int{"quantity": 1}, nil))
		assert.Equal(t, http.StatusBadRequest,
			visitor.do("POST", "/carts/wishlist/items/"+skuB+"/move", map[string]any{"to_cart_type": "wishlist"}, nil))
		require.Len(t, outbox("shop-a", events.TypeCartItemMovedV1), 1)
	})

	alice := uuid.NewString()
	aliceC := as(t, srv, "shop-a", alice, "customer")

	t.Run("a visitor's shopping cart merges into the customer's on login", func(t *testing.T) {
		code, _ := add(aliceC, "shopping", skuA, 1)
		require.Equal(t, http.StatusOK, code)
		code, _ = add(aliceC, "shopping", skuC, 2)
		require.Equal(t, http.StatusOK, code)

		login := events.IdentityUserLoggedInV1{
			UserID: alice, Role: "customer", Method: "password", PreviousVisitorID: &visitorID, LoggedInAt: time.Now().UTC(),
		}
		publish("shop-a", events.TypeIdentityUserLoggedInV1, login)
		require.Eventually(t, func() bool { return len(outbox("shop-a", events.TypeCartMergedV1)) == 2 },
			10*time.Second, 50*time.Millisecond, "shopping and wishlist merge")

		c := aliceC.cart("shopping", "")
		require.Len(t, c.Items, 3)
		qty := map[string]int{}
		for _, it := range c.Items {
			qty[it.SkuID.String()] = it.Quantity
		}
		assert.Equal(t, map[string]int{skuA: 4, skuB: 1, skuC: 2}, qty)
		merged := map[string]map[string]any{}
		for _, m := range outbox("shop-a", events.TypeCartMergedV1) {
			merged[m["cart_type"].(string)] = m
		}
		assert.Equal(t, visitorID, merged["shopping"]["visitor_id"])
		assert.Equal(t, float64(2), merged["shopping"]["merged_item_count"])
		assert.Equal(t, float64(1), merged["wishlist"]["merged_item_count"])

		assert.Empty(t, visitor.cart("shopping", "").Items, "the visitor carts are gone")
		assert.Empty(t, visitor.cart("wishlist", "").Items)
		assert.Len(t, aliceC.cart("wishlist", "").Items, 1)

		publish("shop-a", events.TypeIdentityUserLoggedInV1, login)
		time.Sleep(500 * time.Millisecond)
		assert.Len(t, outbox("shop-a", events.TypeCartMergedV1), 2, "a redelivered login merges nothing")
		assert.Len(t, aliceC.cart("shopping", "").Items, 3)
	})

	t.Run("a customer's display currency comes from their preferences", func(t *testing.T) {
		publish("shop-a", events.TypePreferenceCurrencyChangedV1, events.PreferenceCurrencyChangedV1{
			UserID: alice, DisplayCurrency: "USD", ChangedAt: time.Now().UTC(),
		})
		require.Eventually(t, func() bool { return aliceC.cart("shopping", "").Display != nil },
			10*time.Second, 50*time.Millisecond)
		c := aliceC.cart("shopping", "?display_currency=ZAR")
		assert.Nil(t, c.Display, "an explicit display currency wins")

		publish("shop-a", events.TypePreferenceCurrencyChangedV1, events.PreferenceCurrencyChangedV1{
			UserID: alice, DisplayCurrency: "ZAR", ChangedAt: time.Now().Add(-time.Hour).UTC(),
		})
		time.Sleep(500 * time.Millisecond)
		assert.NotNil(t, aliceC.cart("shopping", "").Display, "an older change is ignored")
	})

	t.Run("deleted SKUs leave every cart", func(t *testing.T) {
		publish("shop-a", events.TypeProductSKUDeletedV1, events.ProductSKUDeletedV1{
			SKUID: skuB, ProductID: uuid.NewString(), DeletedAt: time.Now().UTC(),
		})
		require.Eventually(t, func() bool { return len(aliceC.cart("shopping", "").Items) == 2 },
			10*time.Second, 50*time.Millisecond)
		assert.Empty(t, aliceC.cart("wishlist", "").Items)
		removed := outbox("shop-a", events.TypeCartItemRemovedV1)
		require.Len(t, removed, 2)
		assert.Equal(t, "sku_deleted", removed[0]["reason"])
	})

	t.Run("items are removed and carts cleared", func(t *testing.T) {
		assert.Equal(t, http.StatusNoContent, aliceC.do("DELETE", "/carts/shopping/items/"+skuC, nil, nil))
		assert.Equal(t, http.StatusNotFound, aliceC.do("DELETE", "/carts/shopping/items/"+skuC, nil, nil))
		assert.Equal(t, http.StatusNoContent, aliceC.do("DELETE", "/carts/shopping", nil, nil))
		assert.Empty(t, aliceC.cart("shopping", "").Items)
		cleared := outbox("shop-a", events.TypeCartClearedV1)
		require.Len(t, cleared, 1)
		assert.Equal(t, float64(1), cleared[0]["item_count"])
	})

	t.Run("checkout reads a cart and a completed checkout removes its quantities", func(t *testing.T) {
		checkout := as(t, srv, "shop-a", "yc-checkout", "service")
		checkout.hdr[authctx.HeaderCallerService] = "yc-checkout"
		var empty api.InternalCart
		require.Equal(t, http.StatusOK, checkout.do("GET", "/internal/carts/"+uuid.NewString()+"/shopping", nil, &empty))
		assert.Nil(t, empty.CartID)
		assert.Empty(t, empty.Items)
		assert.Equal(t, http.StatusNotFound, checkout.do("GET", "/internal/carts/"+alice+"/layaway", nil, nil))
		assert.Equal(t, http.StatusUnauthorized, aliceC.do("GET", "/internal/carts/"+alice+"/shopping", nil, nil), "service callers only")

		code, _ := add(aliceC, "shopping", skuA, 3)
		require.Equal(t, http.StatusOK, code)
		code, _ = add(aliceC, "shopping", skuC, 1)
		require.Equal(t, http.StatusOK, code)
		var ic api.InternalCart
		require.Equal(t, http.StatusOK, checkout.do("GET", "/internal/carts/"+alice+"/shopping", nil, &ic))
		require.NotNil(t, ic.CartID)
		assert.True(t, ic.Convertible)
		require.Len(t, ic.Items, 2)
		assert.Equal(t, skuA, ic.Items[0].SkuID.String())
		assert.Equal(t, 3, ic.Items[0].Quantity)
		assert.Equal(t, money.New(10000, "ZAR"), ic.Items[0].UnitPrice)

		// The session checked out 2 of skuA and 1 of skuC; one skuA was
		// added after the session started and stays.
		session, order := uuid.NewString(), uuid.NewString()
		completed := events.CheckoutCompletedV1{
			SessionID: session, UserID: alice, CartType: "shopping", OrderID: order, OrderNumber: "SHOP-A-000001",
			Items: []events.CheckoutCompletedV1ItemsItem{{SKUID: skuA, Quantity: 2}, {SKUID: skuC, Quantity: 1}},
			Total: events.Money{AmountMinor: 20400, Currency: "ZAR"}, CompletedAt: time.Now().UTC(),
		}
		publish("shop-a", events.TypeCheckoutCompletedV1, completed)
		require.Eventually(t, func() bool { return len(outbox("shop-a", events.TypeCartConvertedV1)) == 1 },
			10*time.Second, 50*time.Millisecond)
		c := aliceC.cart("shopping", "")
		require.Len(t, c.Items, 1)
		assert.Equal(t, skuA, c.Items[0].SkuID.String())
		assert.Equal(t, 1, c.Items[0].Quantity)
		converted := outbox("shop-a", events.TypeCartConvertedV1)[0]
		assert.Equal(t, session, converted["session_id"])
		assert.Equal(t, order, converted["order_id"])
		assert.Len(t, converted["items"], 2)

		publish("shop-a", events.TypeCheckoutCompletedV1, completed)
		time.Sleep(500 * time.Millisecond)
		assert.Len(t, outbox("shop-a", events.TypeCartConvertedV1), 1, "a session converts once")
		assert.Equal(t, 1, aliceC.cart("shopping", "").Items[0].Quantity)
	})

	t.Run("carts are for shoppers in their own shop", func(t *testing.T) {
		admin := as(t, srv, "shop-a", uuid.NewString(), "shop_admin")
		assert.Equal(t, http.StatusForbidden, admin.do("GET", "/carts", nil, nil))
		other := as(t, srv, "shop-b", alice, "customer")
		assert.Empty(t, other.cart("shopping", "").Items)
	})

	t.Run("customers' idle shopping carts are announced as abandoned once", func(t *testing.T) {
		bob, erin, carol := uuid.NewString(), uuid.NewString(), uuid.NewString()
		bobC := as(t, srv, "shop-a", bob, "customer")
		code, _ := add(bobC, "shopping", skuA, 2)
		require.Equal(t, http.StatusOK, code)
		code, _ = add(bobC, "wishlist", skuB, 1)
		require.Equal(t, http.StatusOK, code)
		code, _ = add(as(t, srv, "shop-a", erin, "customer"), "shopping", skuC, 1)
		require.Equal(t, http.StatusOK, code)
		code, _ = add(as(t, srv, "shop-a", carol, "visitor"), "shopping", skuC, 1)
		require.Equal(t, http.StatusOK, code)
		idle := func(users ...string) {
			t.Helper()
			require.NoError(t, dbkit.WithTenantTx(ctx, db, "shop-a", func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE carts SET updated_at = now() - interval '2 hours' WHERE user_id = ANY($1)`, users)
				return err
			}))
		}
		idle(bob, erin, carol)

		n, err := a.Service.SweepAbandoned(ctx)
		require.NoError(t, err)
		assert.Equal(t, 2, n, "only customers' shopping carts; wishlists and visitors' carts are not")
		abandoned := outbox("shop-a", events.TypeCartAbandonedV1)
		require.Len(t, abandoned, 2)
		byUser := map[string]map[string]any{}
		for _, p := range abandoned {
			byUser[p["user_id"].(string)] = p
		}
		require.Contains(t, byUser, bob)
		assert.Equal(t, "shopping", byUser[bob]["cart_type"])
		assert.Equal(t, float64(1), byUser[bob]["item_count"])
		assert.Equal(t, float64(2), byUser[bob]["quantity"])

		n, err = a.Service.SweepAbandoned(ctx)
		require.NoError(t, err)
		assert.Zero(t, n, "a cart is announced once per idle spell")

		admin := as(t, srv, "shop-a", uuid.NewString(), "shop_admin")
		var page api.AbandonedCartList
		require.Equal(t, http.StatusOK, admin.do("GET", "/admin/abandoned-carts?page_size=1", nil, &page))
		require.Len(t, page.Items, 1)
		require.NotEmpty(t, page.Page.NextCursor)
		first, cursor := page.Items[0].UserID.String(), page.Page.NextCursor
		page = api.AbandonedCartList{}
		require.Equal(t, http.StatusOK, admin.do("GET", "/admin/abandoned-carts?page_size=1&cursor="+url.QueryEscape(cursor), nil, &page))
		require.Len(t, page.Items, 1)
		assert.NotEqual(t, first, page.Items[0].UserID.String())
		assert.Empty(t, page.Page.NextCursor)
		assert.Equal(t, http.StatusForbidden, bobC.do("GET", "/admin/abandoned-carts", nil, nil))

		code, _ = add(bobC, "shopping", skuC, 1)
		require.Equal(t, http.StatusOK, code)
		page = api.AbandonedCartList{}
		require.Equal(t, http.StatusOK, admin.do("GET", "/admin/abandoned-carts", nil, &page))
		require.Len(t, page.Items, 1, "using the cart clears its abandonment")
		assert.Equal(t, erin, page.Items[0].UserID.String())

		idle(bob)
		n, err = a.Service.SweepAbandoned(ctx)
		require.NoError(t, err)
		assert.Equal(t, 1, n, "a new idle spell is announced again")
		assert.Len(t, outbox("shop-a", events.TypeCartAbandonedV1), 3)
	})

	t.Run("a deleted user's carts are deleted (GDPR)", func(t *testing.T) {
		dave := uuid.NewString()
		daveC := as(t, srv, "shop-a", dave, "customer")
		code, _ := add(daveC, "shopping", skuA, 1)
		require.Equal(t, http.StatusOK, code)
		code, _ = add(daveC, "wishlist", skuC, 1)
		require.Equal(t, http.StatusOK, code)
		publish("shop-a", events.TypeIdentityUserDeletedV1, events.IdentityUserDeletedV1{
			UserID: dave, Pseudonym: uuid.NewString(), Role: "customer", RequestedBy: "user", DeletedAt: time.Now().UTC(),
		})
		require.Eventually(t, func() bool { return len(outbox("shop-a", events.TypePlatformGdprCascadeCompletedV1)) == 1 },
			10*time.Second, 50*time.Millisecond)
		done := outbox("shop-a", events.TypePlatformGdprCascadeCompletedV1)[0]
		assert.Equal(t, "yc-cart", done["service"])
		assert.Equal(t, "deleted", done["action"])
		assert.EqualValues(t, 2, done["record_count"])
		var left int
		require.NoError(t, dbkit.WithTenantTx(ctx, db, "shop-a", func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM carts WHERE user_id = $1`, dave).Scan(&left)
		}))
		assert.Zero(t, left)
	})
}

func unique(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
