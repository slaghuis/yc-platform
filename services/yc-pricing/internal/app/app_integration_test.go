//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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
	money "github.com/slaghuis/yc-platform/libs/yc-money"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	testkit "github.com/slaghuis/yc-platform/libs/yc-testkit"
	api "github.com/slaghuis/yc-platform/services/yc-pricing/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-pricing/internal/app"
)

type client struct {
	t   *testing.T
	srv *httptest.Server
	hdr map[string]string
}

// as returns a client sending the headers yc-gateway would set.
func as(t *testing.T, srv *httptest.Server, tenant, actorType string) client {
	return client{t: t, srv: srv, hdr: map[string]string{
		authctx.HeaderTenantID:  tenant,
		authctx.HeaderActorID:   "00000000-0000-0000-0000-0000000000a1",
		authctx.HeaderActorType: actorType,
	}}
}

func (c client) with(key, value string) client {
	h := map[string]string{key: value}
	for k, v := range c.hdr {
		h[k] = v
	}
	return client{t: c.t, srv: c.srv, hdr: h}
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

// fakeFrankfurter serves ZAR rates until failing is set.
func fakeFrankfurter(t *testing.T, failing *atomic.Bool) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() || r.URL.Path != "/latest" || r.URL.Query().Get("base") != "ZAR" {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"amount":1.0,"base":"ZAR","date":"2026-10-14","rates":{"EUR":0.05,"USD":0.055,"JPY":8.25}}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func zar(minor int64) money.Money { return money.New(minor, "ZAR") }

func TestPricing(t *testing.T) {
	ctx := context.Background()
	logs := telemetry.InitForTest(t, "yc-pricing")
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	})

	var ratesDown atomic.Bool
	dbURL := testkit.Postgres(t)
	natsURL := testkit.NATS(t)
	t.Setenv("YC_PRICING_DB_URL", dbURL)
	t.Setenv("NATS_URL", natsURL)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("YC_PRICING__RATES__FRANKFURTER_URL", fakeFrankfurter(t, &ratesDown))
	t.Setenv("YC_PRICING__RATES__STALE_AFTER", "1s")
	cfg, err := app.LoadConfig()
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())
	a, err := app.New(ctx, cfg, httpkit.BuildInfo{Service: "yc-pricing", Version: "test"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	srv := httptest.NewServer(a.Handler)
	t.Cleanup(srv.Close)

	db, err := pgxpool.New(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(db.Close)
	eventsOf := func(tenant, eventType string) int {
		var n int
		require.NoError(t, db.QueryRow(ctx, `SELECT count(*) FROM outbox
			WHERE tenant_id = $1 AND envelope->>'event_type' = $2`, tenant, eventType).Scan(&n))
		return n
	}
	syncedSKUs := func(tenant string, skuIDs ...string) int {
		tx, err := db.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()
		_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant)
		require.NoError(t, err)
		var n int
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM sku_tax_classes
			WHERE sku_id = ANY($1::uuid[]) AND status <> 'deleted'`, skuIDs).Scan(&n))
		return n
	}

	validator, err := events.NewValidator()
	require.NoError(t, err)
	upstream, err := eventbus.NewClient(ctx, eventbus.Config{
		NATSURL: natsURL, ServiceName: "yc-product", Validator: validator,
		Streams: []eventbus.StreamConfig{eventbus.PlatformStream(), eventbus.ProductStream()},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upstream.Close(context.Background()) })
	publish := func(tenant, eventType string, payload any) {
		t.Helper()
		require.NoError(t, upstream.PublishDirect(ctx, eventbus.Event{
			EventType: eventType, TenantID: tenant,
			Actor: eventbus.Actor{Type: "service", ID: "yc-product"}, Payload: payload,
		}))
	}
	skuCreated := func(skuID, taxClass string) {
		publish("shop-a", events.TypeProductSKUCreatedV1, events.ProductSKUCreatedV1{
			SKUID: skuID, ProductID: uuid.NewString(), SKUCode: "SKU-" + skuID[:8], VariantValues: map[string]any{},
			IsInventoryTracked: true, TaxClass: taxClass, Status: "active", CreatedAt: time.Now().UTC(),
		})
	}

	admin := as(t, srv, "shop-a", "shop_admin")
	visitor := as(t, srv, "shop-a", "visitor")
	shirt, book, mug, unknown := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()

	t.Run("provisioned shop gets its pricing config", func(t *testing.T) {
		publish(eventbus.PlatformTenantID, events.TypePlatformTenantProvisionedV1, events.PlatformTenantProvisionedV1{
			TenantID: "shop-a", Name: "Shop A", Hostnames: []string{"shop-a.test"},
			StoreCurrency: "ZAR", DefaultJurisdiction: "ZA", IsTaxInclusive: true, ProvisionedAt: time.Now().UTC(),
		})
		var c api.PricingConfig
		require.Eventually(t, func() bool { return admin.do("GET", "/admin/pricing-config", nil, &c) == http.StatusOK },
			10*time.Second, 50*time.Millisecond)
		assert.Equal(t, "ZAR", c.StoreCurrency)
		assert.True(t, c.TaxInclusive)
		assert.Equal(t, "ZA", c.DefaultJurisdiction)
		assert.Equal(t, 1, eventsOf("shop-a", events.TypePricingConfigChangedV1))
		assert.Equal(t, http.StatusForbidden, visitor.do("GET", "/admin/pricing-config", nil, nil))
	})

	t.Run("shop admin defines tax classes", func(t *testing.T) {
		vat := func(rate string) map[string]any {
			return map[string]any{"display_name": "VAT " + rate, "rules": []map[string]any{
				{"jurisdiction": "ZA", "rule_name": "VAT", "rate": rate, "base": "line_total", "sort_order": 0},
			}}
		}
		var c api.TaxClass
		require.Equal(t, http.StatusCreated, admin.do("PUT", "/admin/tax-classes/za-standard", vat("0.14"), &c))
		require.Equal(t, http.StatusOK, admin.do("PUT", "/admin/tax-classes/za-standard", vat("0.15"), &c))
		assert.Equal(t, "0.15", c.Rules[0].Rate)
		require.Equal(t, http.StatusCreated, admin.do("PUT", "/admin/tax-classes/za-zero", vat("0"), nil))
		assert.Equal(t, 2, eventsOf("shop-a", events.TypePricingTaxClassCreatedV1))
		assert.Equal(t, 1, eventsOf("shop-a", events.TypePricingTaxClassUpdatedV1))

		bad := vat("0.15")
		bad["rules"].([]map[string]any)[0]["base"] = "line_total_including_GST"
		assert.Equal(t, http.StatusBadRequest, admin.do("PUT", "/admin/tax-classes/za-bad", bad, nil))
		assert.Equal(t, http.StatusBadRequest, admin.do("PUT", "/admin/tax-classes/ZA%20Bad", vat("0.15"), nil))

		var list api.TaxClassList
		require.Equal(t, http.StatusOK, admin.do("GET", "/admin/tax-classes", nil, &list))
		require.Len(t, list.Items, 2)
		assert.Equal(t, "za-standard", list.Items[0].ClassID)
	})

	t.Run("catalog events give SKUs their tax class", func(t *testing.T) {
		skuCreated(shirt, "za-standard")
		skuCreated(book, "za-zero")
		require.Eventually(t, func() bool { return syncedSKUs("shop-a", shirt, book) == 2 }, 10*time.Second, 50*time.Millisecond)
		assert.Equal(t, http.StatusConflict, admin.do("DELETE", "/admin/tax-classes/za-zero", nil, nil), "in use")
	})

	t.Run("shop admin sets base prices", func(t *testing.T) {
		var p api.BasePrice
		require.Equal(t, http.StatusOK, admin.do("PUT", "/admin/prices/sku/"+shirt, map[string]any{"price": zar(20000)}, &p))
		assert.True(t, p.TaxInclusive)
		require.Equal(t, http.StatusOK, admin.do("PUT", "/admin/prices/sku/"+shirt, map[string]any{"price": zar(25000)}, &p))
		require.Equal(t, http.StatusOK, admin.do("PUT", "/admin/prices/sku/"+shirt, map[string]any{"price": zar(25000)}, &p), "unchanged")
		require.Equal(t, http.StatusOK, admin.do("PUT", "/admin/prices/sku/"+book, map[string]any{"price": zar(10000)}, nil))
		assert.Equal(t, 3, eventsOf("shop-a", events.TypePricingBasePriceSetV1))

		assert.Equal(t, http.StatusBadRequest, admin.do("PUT", "/admin/prices/sku/"+shirt,
			map[string]any{"price": money.New(100, "USD")}, nil), "not the store currency")
		assert.Equal(t, http.StatusForbidden, visitor.do("PUT", "/admin/prices/sku/"+shirt, map[string]any{"price": zar(1)}, nil))

		var hist api.PriceHistory
		require.Equal(t, http.StatusOK, admin.do("GET", "/admin/prices/sku/"+shirt, nil, &hist))
		require.Len(t, hist.Items, 2)
		assert.Equal(t, zar(25000), hist.Items[0].Price)
		assert.Nil(t, hist.Items[0].ValidTo)
		assert.NotNil(t, hist.Items[1].ValidTo)

		var list api.BasePriceList
		require.Equal(t, http.StatusOK, admin.do("GET", "/admin/prices?page_size=1", nil, &list))
		require.Len(t, list.Items, 1)
		require.NotEmpty(t, list.Page.NextCursor)
		var last api.BasePriceList
		require.Equal(t, http.StatusOK, admin.do("GET", "/admin/prices?page_size=1&cursor="+list.Page.NextCursor, nil, &last))
		require.Len(t, last.Items, 1)
		assert.NotEqual(t, list.Items[0].SkuID, last.Items[0].SkuID)
		assert.Empty(t, last.Page.NextCursor)
	})

	t.Run("visitor gets prices with the VAT breakdown and audit trail", func(t *testing.T) {
		var res api.PricingResult
		require.Equal(t, http.StatusOK, visitor.do("POST", "/prices/evaluate", map[string]any{"lines": []map[string]any{
			{"sku_id": shirt, "quantity": 2}, {"sku_id": book, "quantity": 1}, {"sku_id": unknown, "quantity": 1},
		}}, &res))
		assert.Equal(t, "ZAR", res.StoreCurrency)
		assert.Equal(t, "ZA", res.Jurisdiction)
		require.Len(t, res.Lines, 2)
		assert.Equal(t, zar(50000), res.Lines[0].Gross, "2 × R250.00 incl. VAT")
		assert.Equal(t, zar(43478), res.Lines[0].Net)
		assert.Equal(t, zar(6522), res.Lines[0].Tax)
		assert.Equal(t, zar(10000), res.Lines[1].Gross, "zero-rated")
		assert.Equal(t, zar(0), res.Lines[1].Tax)
		assert.Equal(t, []api.Unpriced{{SkuID: uuid.MustParse(unknown), Reason: "no_base_price"}}, res.Unpriced)

		assert.Equal(t, zar(53478), res.Totals.SubtotalExclTax)
		assert.Equal(t, zar(6522), res.Totals.TaxTotal)
		assert.Equal(t, zar(60000), res.Totals.GrandTotal)
		assert.Equal(t, []api.TaxTotal{
			{Rule: "VAT", Rate: "0.15", Amount: zar(6522)},
			{Rule: "VAT", Rate: "0", Amount: zar(0)},
		}, res.Totals.TaxBreakdown)
		assert.Nil(t, res.Display)
		require.Len(t, res.AuditTrail, 2)
		assert.Equal(t, "base_price", res.AuditTrail[0].Evaluator)
		assert.Equal(t, "tax", res.AuditTrail[1].Evaluator)

		assert.Equal(t, http.StatusNotFound, visitor.do("GET", "/prices/sku/"+unknown, nil, nil))
		assert.Equal(t, http.StatusBadRequest, visitor.do("POST", "/prices/evaluate",
			map[string]any{"lines": []map[string]any{{"sku_id": shirt, "quantity": 0}}}, nil))
	})

	t.Run("display currency converts with fetched rates", func(t *testing.T) {
		require.Eventually(t, func() bool {
			return eventsOf(eventbus.PlatformTenantID, events.TypePricingExchangeRatesUpdatedV1) == 1
		}, 10*time.Second, 50*time.Millisecond, "a new store currency is fetched without waiting for the schedule")

		var res api.PricingResult
		require.Equal(t, http.StatusOK, visitor.with("X-YC-Display-Currency", "USD").do("GET", "/prices/sku/"+shirt, nil, &res))
		require.NotNil(t, res.Display)
		assert.Equal(t, "USD", res.Display.Currency)
		assert.Equal(t, "0.055", res.Display.Rate)
		assert.Equal(t, money.New(1375, "USD"), res.Display.Totals.GrandTotal, "R250.00 at 0.055")
		assert.Equal(t, zar(25000), res.Totals.GrandTotal, "store amounts are unchanged")
		require.Len(t, res.AuditTrail, 3)
		assert.Equal(t, "display_currency", res.AuditTrail[2].Evaluator)

		require.Equal(t, http.StatusOK, visitor.with("X-YC-Display-Currency", "USD").do("GET", "/prices/sku/"+shirt+"?display_currency=JPY", nil, &res))
		assert.Equal(t, money.New(2063, "JPY"), res.Display.Totals.GrandTotal, "query wins; JPY has no minor unit")

		assert.Equal(t, http.StatusBadRequest, visitor.do("GET", "/prices/sku/"+shirt+"?display_currency=GBP", nil, nil), "no rate")
	})

	t.Run("prices entered excluding tax get VAT added", func(t *testing.T) {
		var c api.PricingConfig
		require.Equal(t, http.StatusOK, admin.do("PUT", "/admin/pricing-config",
			map[string]any{"store_currency": "ZAR", "tax_inclusive": false, "default_jurisdiction": "ZA"}, &c))
		assert.False(t, c.TaxInclusive)
		assert.Equal(t, 2, eventsOf("shop-a", events.TypePricingConfigChangedV1))
		assert.Equal(t, http.StatusConflict, admin.do("PUT", "/admin/pricing-config",
			map[string]any{"store_currency": "USD", "tax_inclusive": false, "default_jurisdiction": "ZA"}, nil),
			"currency is fixed once prices exist")

		skuCreated(mug, "za-standard")
		require.Equal(t, http.StatusOK, admin.do("PUT", "/admin/prices/sku/"+mug, map[string]any{"price": zar(10000)}, nil))
		var res api.PricingResult
		require.Eventually(t, func() bool {
			return visitor.do("GET", "/prices/sku/"+mug, nil, &res) == http.StatusOK
		}, 10*time.Second, 50*time.Millisecond)
		assert.False(t, res.TaxInclusive)
		assert.False(t, res.Lines[0].PriceIncludesTax)
		assert.Equal(t, zar(10000), res.Lines[0].Net)
		assert.Equal(t, zar(11500), res.Lines[0].Gross)

		require.Equal(t, http.StatusOK, visitor.do("GET", "/prices/sku/"+shirt, nil, &res))
		assert.True(t, res.Lines[0].PriceIncludesTax, "earlier prices keep their meaning")
		assert.Equal(t, zar(25000), res.Lines[0].Gross)
		assert.Equal(t, 1, eventsOf(eventbus.PlatformTenantID, events.TypePricingExchangeRatesUpdatedV1),
			"fresh rates are not fetched again on a config change")
	})

	t.Run("stale rates are reported once", func(t *testing.T) {
		ratesDown.Store(true)
		time.Sleep(1100 * time.Millisecond)
		require.Error(t, a.Rates.RefreshAll(ctx))
		require.Error(t, a.Rates.RefreshAll(ctx))
		assert.Equal(t, 1, eventsOf(eventbus.PlatformTenantID, events.TypePricingExchangeRatesStaleV1))

		var res api.PricingResult
		require.Equal(t, http.StatusOK, visitor.do("GET", "/prices/sku/"+shirt+"?display_currency=USD", nil, &res),
			"the last known rate is still used")
	})

	t.Run("deleted SKUs lose their price", func(t *testing.T) {
		publish("shop-a", events.TypeProductSKUDeletedV1, events.ProductSKUDeletedV1{
			SKUID: book, ProductID: uuid.NewString(), DeletedAt: time.Now().UTC(),
		})
		require.Eventually(t, func() bool {
			return visitor.do("GET", "/prices/sku/"+book, nil, nil) == http.StatusNotFound
		}, 10*time.Second, 50*time.Millisecond)
		assert.Equal(t, 1, eventsOf("shop-a", events.TypePricingBasePriceRemovedV1))
		assert.Equal(t, http.StatusNoContent, admin.do("DELETE", "/admin/tax-classes/za-zero", nil, nil), "no longer in use")
	})

	t.Run("other shops see nothing", func(t *testing.T) {
		other := as(t, srv, "shop-b", "visitor")
		assert.Equal(t, http.StatusConflict, other.do("GET", "/prices/sku/"+shirt, nil, nil), "shop-b has no pricing config")
		assert.Equal(t, http.StatusBadRequest, as(t, srv, "_platform", "system_admin").do("GET", "/prices/sku/"+shirt, nil, nil))
	})
}
