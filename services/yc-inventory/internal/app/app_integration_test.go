//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	testkit "github.com/slaghuis/yc-platform/libs/yc-testkit"
	api "github.com/slaghuis/yc-platform/services/yc-inventory/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-inventory/internal/app"
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

// checkout is yc-checkout calling through the gateway's internal listener.
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
	if out != nil {
		require.NoError(c.t, json.NewDecoder(resp.Body).Decode(out))
	}
	return resp.StatusCode, resp.Header
}

func (c client) code(method, path string, body any, header ...string) int {
	c.t.Helper()
	code, _ := c.do(method, path, body, nil, header...)
	return code
}

func TestInventory(t *testing.T) {
	ctx := context.Background()
	logs := telemetry.InitForTest(t, "yc-inventory")
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	})

	dbURL := testkit.Postgres(t)
	natsURL := testkit.NATS(t)
	t.Setenv("YC_INVENTORY_DB_URL", dbURL)
	t.Setenv("NATS_URL", natsURL)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("YC_INVENTORY__RESERVATION__SWEEPER_INTERVAL", "100ms")
	cfg, err := app.LoadConfig()
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())
	a, err := app.New(ctx, cfg, httpkit.BuildInfo{Service: "yc-inventory", Version: "test"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	srv := httptest.NewServer(a.Handler)
	t.Cleanup(srv.Close)

	db, err := pgxpool.New(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(db.Close)
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
	skusIn := func(evts []map[string]any) []string {
		out := make([]string, 0, len(evts))
		for _, e := range evts {
			out = append(out, e["sku_id"].(string))
		}
		return out
	}

	validator, err := events.NewValidator()
	require.NoError(t, err)
	upstream, err := eventbus.NewClient(ctx, eventbus.Config{
		NATSURL: natsURL, ServiceName: "yc-upstream", Validator: validator,
		Streams: []eventbus.StreamConfig{eventbus.PlatformStream(), eventbus.ProductStream()},
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
	skuEvent := func(sku string, tracked bool) events.ProductSKUCreatedV1 {
		return events.ProductSKUCreatedV1{
			SKUID: sku, ProductID: uuid.NewString(), SKUCode: sku[:8], VariantValues: map[string]any{},
			IsInventoryTracked: tracked, TaxClass: "za-standard", Status: "active", CreatedAt: time.Now().UTC(),
		}
	}

	admin := as(t, srv, "shop-a", uuid.NewString(), "shop_admin")
	svc := checkout(t, srv, "shop-a")
	stock := func(sku string) api.StockLevel {
		t.Helper()
		var l api.StockLevel
		code, _ := admin.do("GET", "/admin/stock/"+sku, nil, &l)
		require.Equal(t, http.StatusOK, code)
		return l
	}
	skuA, skuB, untracked := uuid.NewString(), uuid.NewString(), uuid.NewString()

	t.Run("a provisioned shop gets the default settings", func(t *testing.T) {
		var c api.InventoryConfig
		code, _ := admin.do("GET", "/admin/inventory/config", nil, &c)
		require.Equal(t, http.StatusOK, code)
		assert.Nil(t, c.UpdatedAt, "the service defaults until provisioned")
		publish(eventbus.PlatformTenantID, events.TypePlatformTenantProvisionedV1, events.PlatformTenantProvisionedV1{
			TenantID: "shop-a", Name: "Shop A", Hostnames: []string{"a.test"}, StoreCurrency: "ZAR",
			DefaultJurisdiction: "ZA", IsTaxInclusive: true, ProvisionedAt: time.Now().UTC(),
		})
		require.Eventually(t, func() bool {
			var c api.InventoryConfig
			admin.do("GET", "/admin/inventory/config", nil, &c)
			return c.UpdatedAt != nil
		}, 10*time.Second, 50*time.Millisecond)
		admin.do("GET", "/admin/inventory/config", nil, &c)
		assert.True(t, c.InventoryEnabled)
		assert.Equal(t, 15, c.DefaultReservationTTLMinutes)
	})

	t.Run("tracked SKUs start at zero and are depleted", func(t *testing.T) {
		publish("shop-a", events.TypeProductSKUCreatedV1, skuEvent(skuA, true))
		publish("shop-a", events.TypeProductSKUCreatedV1, skuEvent(skuB, true))
		publish("shop-a", events.TypeProductSKUCreatedV1, skuEvent(untracked, false))
		require.Eventually(t, func() bool { return len(outbox("shop-a", events.TypeInventoryStockDepletedV1)) == 2 },
			10*time.Second, 50*time.Millisecond)
		assert.ElementsMatch(t, []string{skuA, skuB}, skusIn(outbox("shop-a", events.TypeInventoryStockDepletedV1)))
		assert.Equal(t, http.StatusNotFound, admin.code("GET", "/admin/stock/"+untracked, nil))
		var list api.StockList
		code, _ := admin.do("GET", "/admin/stock?page_size=1", nil, &list)
		require.Equal(t, http.StatusOK, code)
		require.Len(t, list.Items, 1)
		require.NotEmpty(t, list.Page.NextCursor)
		first := list.Items[0].SkuID
		var page2 api.StockList
		code, _ = admin.do("GET", "/admin/stock?page_size=1&cursor="+list.Page.NextCursor, nil, &page2)
		require.Equal(t, http.StatusOK, code)
		require.Len(t, page2.Items, 1)
		assert.NotEqual(t, first, page2.Items[0].SkuID)
		assert.Empty(t, page2.Page.NextCursor)
	})

	t.Run("admins receive and adjust stock", func(t *testing.T) {
		var l api.StockLevel
		code, _ := admin.do("POST", "/admin/stock/"+skuA+"/receive", map[string]any{"quantity": 10, "reason": "PO-1"}, &l)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, int64(10), l.Available)
		require.Equal(t, http.StatusOK, admin.code("POST", "/admin/stock/"+skuB+"/receive", map[string]any{"quantity": 1}))
		assert.ElementsMatch(t, []string{skuA, skuB}, skusIn(outbox("shop-a", events.TypeInventoryStockRestoredV1)))

		code, _ = admin.do("POST", "/admin/stock/"+skuA+"/adjust", map[string]any{"delta": -1, "reason": "count"}, &l)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, int64(9), l.OnHand)
		assert.Equal(t, http.StatusBadRequest, admin.code("POST", "/admin/stock/"+skuA+"/adjust", map[string]any{"delta": -1}))
		assert.Equal(t, http.StatusBadRequest,
			admin.code("POST", "/admin/stock/"+skuA+"/adjust", map[string]any{"delta": -100, "reason": "x"}))
		assert.Equal(t, http.StatusNotFound,
			admin.code("POST", "/admin/stock/"+untracked+"/receive", map[string]any{"quantity": 1}))
		code, _ = admin.do("PUT", "/admin/stock/"+skuA+"/low-threshold", map[string]any{"low_threshold": 5}, &l)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, int64(5), l.LowThreshold)
		adjusted := outbox("shop-a", events.TypeInventoryStockAdjustedV1)
		require.Len(t, adjusted, 1)
		assert.Equal(t, "count", adjusted[0]["reason"])
	})

	var resID string
	session := uuid.NewString()
	t.Run("checkout reserves tracked lines and a repeated key replays", func(t *testing.T) {
		body := map[string]any{"session_id": session, "items": []map[string]any{
			{"sku_id": skuA, "quantity": 3}, {"sku_id": untracked, "quantity": 2},
		}}
		var r api.ReserveResult
		code, _ := svc.do("POST", "/internal/reservations", body, &r, httpkit.HeaderIdempotencyKey, session+":reserve")
		require.Equal(t, http.StatusOK, code)
		require.NotNil(t, r.ReservationID)
		resID = r.ReservationID.String()
		assert.Equal(t, []api.Line{{SkuID: uuid.MustParse(skuA), Quantity: 3}}, r.Items, "untracked lines need no stock")
		assert.WithinDuration(t, time.Now().Add(15*time.Minute), *r.ExpiresAt, time.Minute)

		var again api.ReserveResult
		code, h := svc.do("POST", "/internal/reservations", body, &again, httpkit.HeaderIdempotencyKey, session+":reserve")
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, "true", h.Get(httpkit.HeaderIdempotentReplayed))
		assert.Equal(t, *r.ReservationID, *again.ReservationID)
		assert.Equal(t, int64(3), stock(skuA).Reserved)
		assert.Len(t, outbox("shop-a", events.TypeInventoryStockReservedV1), 1)

		assert.Equal(t, http.StatusBadRequest, svc.code("POST", "/internal/reservations", body), "the key is required")
		assert.Equal(t, http.StatusUnauthorized,
			admin.code("POST", "/internal/reservations", body, httpkit.HeaderIdempotencyKey, "x"), "service callers only")
	})

	t.Run("a shortage reserves nothing and names the short lines", func(t *testing.T) {
		s := uuid.NewString()
		var p map[string]any
		code, _ := svc.do("POST", "/internal/reservations", map[string]any{"session_id": s, "items": []map[string]any{
			{"sku_id": skuA, "quantity": 1}, {"sku_id": skuB, "quantity": 2},
		}}, &p, httpkit.HeaderIdempotencyKey, s+":reserve")
		require.Equal(t, http.StatusConflict, code)
		assert.Equal(t, "insufficient_stock", p["code"])
		assert.Equal(t, []any{map[string]any{"sku_id": skuB, "requested": float64(2), "available": float64(1)}}, p["shortages"])
		assert.Equal(t, int64(3), stock(skuA).Reserved, "all or nothing")
		failed := outbox("shop-a", events.TypeInventoryStockReservationFailedV1)
		require.Len(t, failed, 1)
		assert.Equal(t, s, failed[0]["session_id"])
	})

	t.Run("nothing tracked means no reservation", func(t *testing.T) {
		s := uuid.NewString()
		var r api.ReserveResult
		code, _ := svc.do("POST", "/internal/reservations", map[string]any{"session_id": s, "items": []map[string]any{
			{"sku_id": untracked, "quantity": 5},
		}}, &r, httpkit.HeaderIdempotencyKey, s+":reserve")
		require.Equal(t, http.StatusOK, code)
		assert.Nil(t, r.ReservationID)
		assert.Empty(t, r.Items)
	})

	t.Run("commit takes stock out of on hand once", func(t *testing.T) {
		var c api.CommitResult
		code, _ := svc.do("POST", "/internal/reservations/"+resID+"/commit", map[string]any{"reference": "order-1"}, &c,
			httpkit.HeaderIdempotencyKey, session+":commit")
		require.Equal(t, http.StatusOK, code)
		assert.Nil(t, c.Oversold)
		l := stock(skuA)
		assert.Equal(t, int64(6), l.OnHand)
		assert.Equal(t, int64(0), l.Reserved)
		code, _ = svc.do("POST", "/internal/reservations/"+resID+"/commit", nil, &c, httpkit.HeaderIdempotencyKey, "other")
		require.Equal(t, http.StatusOK, code, "committing again is a no-op")
		assert.Equal(t, int64(6), stock(skuA).OnHand)
		assert.Len(t, outbox("shop-a", events.TypeInventoryStockCommittedV1), 1)
		assert.Equal(t, http.StatusConflict,
			svc.code("POST", "/internal/reservations/"+resID+"/release", nil, httpkit.HeaderIdempotencyKey, session+":reserve:undo"))
	})

	reserve := func(sku string, quantity int) (string, string) {
		t.Helper()
		s := uuid.NewString()
		var r api.ReserveResult
		code, _ := svc.do("POST", "/internal/reservations", map[string]any{"session_id": s, "items": []map[string]any{
			{"sku_id": sku, "quantity": quantity},
		}}, &r, httpkit.HeaderIdempotencyKey, s+":reserve")
		require.Equal(t, http.StatusOK, code)
		require.NotNil(t, r.ReservationID)
		return s, r.ReservationID.String()
	}

	t.Run("release returns stock and repeats harmlessly", func(t *testing.T) {
		s, id := reserve(skuA, 1) // available 6 → 5, not under the threshold of 5
		assert.Equal(t, int64(1), stock(skuA).Reserved)
		var r api.ReleaseResult
		code, _ := svc.do("POST", "/internal/reservations/"+id+"/release", nil, &r, httpkit.HeaderIdempotencyKey, s+":reserve:undo")
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, api.ReleaseResultStatus("released"), r.Status)
		assert.Equal(t, int64(0), stock(skuA).Reserved)
		code, _ = svc.do("POST", "/internal/reservations/"+id+"/release", nil, &r, httpkit.HeaderIdempotencyKey, "again")
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, api.ReleaseResultStatus("released"), r.Status)
		code, _ = svc.do("POST", "/internal/reservations/"+uuid.NewString()+"/release", nil, &r, httpkit.HeaderIdempotencyKey, "x")
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, api.ReleaseResultStatus("unknown"), r.Status)
		released := outbox("shop-a", events.TypeInventoryStockReservationReleasedV1)
		require.Len(t, released, 1)
		assert.Equal(t, "compensation", released[0]["reason"])
		assert.Equal(t, http.StatusConflict,
			svc.code("POST", "/internal/reservations/"+id+"/commit", nil, httpkit.HeaderIdempotencyKey, s+":commit"))
	})

	t.Run("falling under the threshold warns, and admins release by hand", func(t *testing.T) {
		_, id := reserve(skuA, 2) // available 6 → 4, under 5
		low := outbox("shop-a", events.TypeInventoryStockLowThresholdReachedV1)
		require.Len(t, low, 1)
		assert.Equal(t, float64(4), low[0]["available"])

		var rl api.ReservationList
		code, _ := admin.do("GET", "/admin/stock/"+skuA+"/reservations", nil, &rl)
		require.Equal(t, http.StatusOK, code)
		require.Len(t, rl.Items, 1)
		assert.Equal(t, id, rl.Items[0].ReservationID.String())
		var r api.Reservation
		code, _ = admin.do("POST", "/admin/reservations/"+id+"/release", nil, &r)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, api.ReservationStatus("released"), r.Status)
		assert.Equal(t, http.StatusConflict, admin.code("POST", "/admin/reservations/"+id+"/release", nil))
		released := outbox("shop-a", events.TypeInventoryStockReservationReleasedV1)
		assert.Equal(t, "admin", released[len(released)-1]["reason"])
	})

	t.Run("expired reservations are swept and a late commit oversells", func(t *testing.T) {
		_, expiring := reserve(skuB, 1) // B: on hand 1, available 0
		assert.Contains(t, skusIn(outbox("shop-a", events.TypeInventoryStockDepletedV1)), skuB)
		require.NoError(t, dbkit.WithTenantTx(ctx, db, "shop-a", func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE reservations SET expires_at = NOW() - INTERVAL '1 second' WHERE id = $1`, expiring)
			return err
		}))
		require.Eventually(t, func() bool { return stock(skuB).Reserved == 0 }, 10*time.Second, 50*time.Millisecond)
		expired := outbox("shop-a", events.TypeInventoryStockReservationExpiredV1)
		require.Len(t, expired, 1)
		assert.Equal(t, expiring, expired[0]["reservation_id"])

		reserve(skuB, 1) // someone else takes the unit
		var c api.CommitResult
		code, _ := svc.do("POST", "/internal/reservations/"+expiring+"/commit", nil, &c, httpkit.HeaderIdempotencyKey, "late")
		require.Equal(t, http.StatusOK, code, "payment was taken, so the commit stands")
		require.NotNil(t, c.Oversold)
		assert.Equal(t, []api.Line{{SkuID: uuid.MustParse(skuB), Quantity: 1}}, *c.Oversold)
		l := stock(skuB)
		assert.Equal(t, int64(0), l.OnHand)
		assert.Equal(t, int64(-1), l.Available)
		committed := outbox("shop-a", events.TypeInventoryStockCommittedV1)
		assert.NotEmpty(t, committed[len(committed)-1]["oversold"])
	})

	t.Run("history lists movements newest first", func(t *testing.T) {
		var page api.MovementList
		code, _ := admin.do("GET", "/admin/stock/"+skuA+"/history?page_size=2", nil, &page)
		require.Equal(t, http.StatusOK, code)
		require.Len(t, page.Items, 2)
		assert.Equal(t, api.MovementType("release"), page.Items[0].Type)
		assert.Equal(t, int64(-2), page.Items[0].ReservedDelta)
		var rest api.MovementList
		code, _ = admin.do("GET", "/admin/stock/"+skuA+"/history?page_size=100&cursor="+page.Page.NextCursor, nil, &rest)
		require.Equal(t, http.StatusOK, code)
		require.Len(t, rest.Items, 6) // receive, adjust, reserve, commit, reserve, release
		assert.Equal(t, api.MovementType("receive"), rest.Items[len(rest.Items)-1].Type)
		assert.Equal(t, "PO-1", *rest.Items[len(rest.Items)-1].Reason)
	})

	t.Run("with inventory off nothing is held and every SKU is in stock", func(t *testing.T) {
		restoredBefore := len(outbox("shop-a", events.TypeInventoryStockRestoredV1))
		off := map[string]any{
			"inventory_enabled": false, "default_reservation_ttl_minutes": 15,
			"min_reservation_ttl_minutes": 5, "max_reservation_ttl_minutes": 30,
		}
		require.Equal(t, http.StatusOK, admin.code("PUT", "/admin/inventory/config", off))
		restored := outbox("shop-a", events.TypeInventoryStockRestoredV1)[restoredBefore:]
		assert.Equal(t, []string{skuB}, skusIn(restored), "out-of-stock SKUs are announced")

		s := uuid.NewString()
		var r api.ReserveResult
		code, _ := svc.do("POST", "/internal/reservations", map[string]any{"session_id": s, "items": []map[string]any{
			{"sku_id": skuA, "quantity": 1000},
		}}, &r, httpkit.HeaderIdempotencyKey, s+":reserve")
		require.Equal(t, http.StatusOK, code)
		assert.Nil(t, r.ReservationID)

		depletedBefore := len(outbox("shop-a", events.TypeInventoryStockDepletedV1))
		off["inventory_enabled"] = true
		require.Equal(t, http.StatusOK, admin.code("PUT", "/admin/inventory/config", off))
		assert.Equal(t, []string{skuB}, skusIn(outbox("shop-a", events.TypeInventoryStockDepletedV1)[depletedBefore:]))
		off["min_reservation_ttl_minutes"] = 20
		assert.Equal(t, http.StatusBadRequest, admin.code("PUT", "/admin/inventory/config", off))
	})

	t.Run("untracked and deleted SKUs lose their stock row", func(t *testing.T) {
		upd := events.ProductSKUUpdatedV1{
			SKUID: skuA, ProductID: uuid.NewString(), SKUCode: "a", VariantValues: map[string]any{},
			IsInventoryTracked: false, TaxClass: "za-standard", Status: "active", UpdatedAt: time.Now().UTC(),
		}
		publish("shop-a", events.TypeProductSKUUpdatedV1, upd)
		require.Eventually(t, func() bool { return admin.code("GET", "/admin/stock/"+skuA, nil) == http.StatusNotFound },
			10*time.Second, 50*time.Millisecond)
		publish("shop-a", events.TypeProductSKUDeletedV1, events.ProductSKUDeletedV1{
			SKUID: skuB, ProductID: uuid.NewString(), DeletedAt: time.Now().UTC(),
		})
		require.Eventually(t, func() bool { return admin.code("GET", "/admin/stock/"+skuB, nil) == http.StatusNotFound },
			10*time.Second, 50*time.Millisecond)
	})

	t.Run("stock is for shop admins in their own shop", func(t *testing.T) {
		assert.Equal(t, http.StatusForbidden, as(t, srv, "shop-a", uuid.NewString(), "customer").code("GET", "/admin/stock", nil))
		var list api.StockList
		code, _ := as(t, srv, "shop-b", uuid.NewString(), "shop_admin").do("GET", "/admin/stock", nil, &list)
		require.Equal(t, http.StatusOK, code)
		assert.Empty(t, list.Items)
	})
}
