//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
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
	api "github.com/slaghuis/yc-platform/services/yc-analytics/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-analytics/internal/app"
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
		authctx.HeaderActorID:   uuid.NewString(),
		authctx.HeaderActorType: actorType,
	}}
}

func (c client) send(method, path string, body any) (int, http.Header, []byte) {
	c.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(c.t, err)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, c.srv.URL+path, r)
	require.NoError(c.t, err)
	for k, v := range c.hdr {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.srv.Client().Do(req)
	require.NoError(c.t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(c.t, err)
	if resp.StatusCode >= 400 {
		c.t.Logf("%s %s: %d %s", method, path, resp.StatusCode, raw)
	}
	return resp.StatusCode, resp.Header, raw
}

func (c client) do(method, path string, body, out any) int {
	c.t.Helper()
	status, _, raw := c.send(method, path, body)
	if status < 400 && out != nil {
		require.NoError(c.t, json.Unmarshal(raw, out))
	}
	return status
}

func (c client) get(path string, out any) int { return c.do(http.MethodGet, path, nil, out) }

func zar(minor int64) events.Money { return events.Money{AmountMinor: minor, Currency: "ZAR"} }

func line(sku string, qty, unit int64) events.OrderPlacedV1ItemsItem {
	return events.OrderPlacedV1ItemsItem{
		SKUID: sku, SKUCode: "SKU-" + sku[:4], NameSnapshot: "Product " + sku[:4], Quantity: qty,
		UnitPrice: zar(unit), LineTotal: zar(qty * unit),
	}
}

func order(number, user, txID string, at time.Time, shipping int64, items ...events.OrderPlacedV1ItemsItem) events.OrderPlacedV1 {
	var sum int64
	for _, it := range items {
		sum += it.LineTotal.AmountMinor
	}
	o := events.OrderPlacedV1{
		OrderID: uuid.NewString(), OrderNumber: number, UserID: user, UserType: "customer",
		CheckoutSessionID: uuid.NewString(), Items: items, RequiresShipping: shipping > 0,
		ItemsTotal: zar(sum), Shipping: zar(shipping), TaxTotal: zar((sum + shipping) * 15 / 115),
		Total: zar(sum + shipping), PlacedAt: at,
	}
	if txID != "" {
		o.PaymentTransactionID = &txID
	}
	return o
}

func TestAnalytics(t *testing.T) {
	ctx := context.Background()
	logs := telemetry.InitForTest(t, "yc-analytics")
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	})

	dbURL := testkit.Postgres(t)
	natsURL := testkit.NATS(t)
	t.Setenv("YC_ANALYTICS_DB_URL", dbURL)
	t.Setenv("NATS_URL", natsURL)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("YC_ANALYTICS_REFRESH_INTERVAL", "100ms")
	cfg, err := app.LoadConfig()
	require.NoError(t, err)
	cfg.Milestones.OrderCounts = []int64{3}
	cfg.Milestones.RevenueTiers = []int64{40_000}
	require.NoError(t, cfg.Validate())
	a, err := app.New(ctx, cfg, httpkit.BuildInfo{Service: "yc-analytics", Version: "test"})
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
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		require.NoError(t, dbkit.WithPlatformTx(ctx, db, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, sql, args...).Scan(&n)
		}))
		return n
	}

	validator, err := events.NewValidator()
	require.NoError(t, err)
	upstream, err := eventbus.NewClient(ctx, eventbus.Config{
		NATSURL: natsURL, ServiceName: "yc-upstream", Validator: validator,
		Streams: append([]eventbus.StreamConfig{
			eventbus.CartStream(), eventbus.CheckoutStream(), eventbus.PaymentStream(), eventbus.OrderStream(),
		}, eventbus.GDPRStreams()...),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upstream.Close(context.Background()) })
	publish := func(tenant string, payload interface{ EventType() string }) {
		t.Helper()
		require.NoError(t, upstream.PublishDirect(ctx, eventbus.Event{
			EventType: payload.EventType(), TenantID: tenant,
			Actor: eventbus.Actor{Type: "service", ID: "yc-upstream"}, Payload: payload,
		}))
	}
	recorded := func(orderID string) bool {
		return count(`SELECT COUNT(*) FROM fact_order WHERE order_id = $1`, orderID) == 1
	}

	sysadmin := as(t, srv, "_platform", "system_admin")
	shopA := as(t, srv, "shop-a", "shop_admin")
	shopB := as(t, srv, "shop-b", "shop_admin")
	customer := as(t, srv, "shop-a", "customer")

	now := time.Now().UTC()
	today := now.Format(time.DateOnly)
	skuA, skuB := uuid.NewString(), uuid.NewString()
	buyer := uuid.NewString()
	tx1, tx2, tx3 := uuid.NewString(), uuid.NewString(), uuid.NewString()
	o1 := order("YC-1001", buyer, tx1, now, 1_000, line(skuA, 2, 5_000), line(skuB, 1, 20_000))
	o2 := order("YC-1002", uuid.NewString(), "", now, 0, line(skuA, 1, 5_000))
	o3 := order("YC-1003", uuid.NewString(), tx3, now, 0, line(skuB, 1, 20_000))

	t.Run("facts become aggregates", func(t *testing.T) {
		for _, shop := range []struct{ id, name string }{{"shop-a", "Shop A"}, {"shop-b", "Shop B"}} {
			publish(eventbus.PlatformTenantID, events.PlatformTenantProvisionedV1{
				TenantID: shop.id, Name: shop.name, Hostnames: []string{shop.id + ".example.com"},
				StoreCurrency: "ZAR", DefaultJurisdiction: "ZA", IsTaxInclusive: true, ProvisionedAt: now,
			})
		}
		require.Eventually(t, func() bool { return count(`SELECT COUNT(*) FROM dim_tenant`) == 2 },
			10*time.Second, 50*time.Millisecond)

		for range 3 {
			publish("shop-a", events.IdentityVisitorCreatedV1{
				UserID: uuid.NewString(), CreatedAt: now, TokenExpiresAt: now.Add(time.Hour),
			})
		}
		for _, cartType := range []string{"shopping", "shopping", "wishlist"} {
			publish("shop-a", events.CartCreatedV1{
				CartID: uuid.NewString(), UserID: uuid.NewString(), UserType: "customer", CartType: cartType, CreatedAt: now,
			})
		}
		sessions := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
		for i, s := range sessions {
			publish("shop-a", events.CheckoutStartedV1{
				SessionID: s, UserID: uuid.NewString(), UserType: "customer", CartType: "shopping",
				ItemCount: 1, ExpiresAt: now.Add(time.Hour), StartedAt: now,
			})
			if i < 2 {
				publish("shop-a", events.CheckoutConfirmedV1{
					SessionID: s, UserID: uuid.NewString(), PaymentGateway: "stripe", Total: zar(1), ConfirmedAt: now,
				})
			}
		}
		publish("shop-a", events.PaymentAuthorizedV1{
			TransactionID: tx1, SessionID: sessions[0], Gateway: "stripe", GatewayReference: "pi_1",
			Amount: zar(31_000), AuthorizedAt: now,
		})
		publish("shop-a", events.PaymentCapturedV1{
			TransactionID: tx1, SessionID: sessions[0], Gateway: "stripe", Amount: zar(31_000), CapturedAt: now,
		})
		publish("shop-a", events.PaymentAuthorizationFailedV1{
			TransactionID: tx2, SessionID: sessions[1], Gateway: "stripe", Amount: zar(5_000),
			Reason: "card_declined", FailedAt: now,
		})
		publish("shop-a", events.PaymentChargedV1{
			TransactionID: tx3, SessionID: sessions[2], Gateway: "yoco", Amount: zar(20_000), ChargedAt: now,
		})

		// One at a time, so the milestone each crosses is deterministic.
		for _, o := range []events.OrderPlacedV1{o1, o2, o3} {
			publish("shop-a", o)
			require.Eventually(t, func() bool { return recorded(o.OrderID) }, 10*time.Second, 50*time.Millisecond)
		}
		publish("shop-a", events.OrderCancelledV1{
			OrderID: o2.OrderID, OrderNumber: o2.OrderNumber, UserID: o2.UserID, Reason: "customer", CancelledAt: now,
		})
		publish("shop-a", events.OrderRefundedV1{
			OrderID: o1.OrderID, OrderNumber: o1.OrderNumber, UserID: o1.UserID, Amount: zar(5_000),
			TotalRefunded: zar(5_000), Reason: "damaged", RefundedAt: now,
		})

		var d api.Dashboard
		require.Eventually(t, func() bool {
			d = api.Dashboard{}
			return shopA.get("/admin/analytics/dashboard", &d) == http.StatusOK && d.PendingDays == 0 &&
				len(d.Revenue) == 1 && d.Revenue[0].Cancelled == 1 && d.Revenue[0].RefundedMinor == 5_000 &&
				d.Funnel.Visitors == 3 && d.Funnel.CheckoutsConfirmed == 2 && len(d.Gateways) == 2
		}, 15*time.Second, 100*time.Millisecond)

		rev := d.Revenue[0]
		assert.Equal(t, "ZAR", rev.Currency)
		assert.Equal(t, int64(2), rev.Orders, "the cancelled order is not counted")
		assert.Equal(t, int64(51_000), rev.GrossMinor)
		assert.Equal(t, int64(46_000), rev.NetMinor)
		assert.Equal(t, int64(1_000), rev.ShippingMinor)
		assert.Equal(t, int64(25_500), rev.AverageOrderMinor)

		assert.Equal(t, api.Funnel{
			Visitors: 3, CartsCreated: 2, CheckoutsStarted: 3, CheckoutsConfirmed: 2, OrdersPlaced: 3, ConversionRate: 1,
		}, d.Funnel, "wishlists are not in the funnel")

		require.Len(t, d.TopProducts, 2)
		assert.Equal(t, skuB, d.TopProducts[0].SkuID.String())
		assert.Equal(t, int64(40_000), d.TopProducts[0].RevenueMinor)
		assert.Equal(t, int64(2), d.TopProducts[1].Units, "the cancelled order's unit is not counted")

		stripe, yoco := d.Gateways[0], d.Gateways[1]
		assert.Equal(t, "stripe", stripe.Gateway)
		assert.Equal(t, [4]int64{2, 1, 1, 31_000}, [4]int64{stripe.Attempts, stripe.Successes, stripe.Failures, stripe.AmountMinor})
		assert.InDelta(t, 0.5, stripe.SuccessRate, 1e-9)
		assert.Equal(t, [4]int64{1, 1, 0, 20_000}, [4]int64{yoco.Attempts, yoco.Successes, yoco.Failures, yoco.AmountMinor})

		var byUnits api.ProductList
		require.Equal(t, http.StatusOK, shopA.get("/admin/analytics/products/top?by=units&limit=1", &byUnits))
		require.Len(t, byUnits.Products, 1)
		assert.Equal(t, http.StatusBadRequest, shopA.get("/admin/analytics/products/top?limit=101", nil))

		var series api.RevenueSeries
		require.Equal(t, http.StatusOK, shopA.get("/admin/analytics/revenue?from="+today+"&to="+today, &series))
		require.Len(t, series.Days, 1)
		assert.Equal(t, today, series.Days[0].Date.String())

		var funnel api.FunnelReport
		require.Equal(t, http.StatusOK, shopA.get("/admin/analytics/funnel", &funnel))
		require.Len(t, funnel.Days, 1)
		assert.Equal(t, int64(3), funnel.Total.OrdersPlaced)

		assert.Equal(t, http.StatusBadRequest, shopA.get("/admin/analytics/revenue?from=2026-02-01&to=2026-01-01", nil))
		assert.Equal(t, http.StatusBadRequest, shopA.get("/admin/analytics/revenue?from=2024-01-01&to=2026-01-01", nil))
	})

	t.Run("reports export as CSV", func(t *testing.T) {
		status, hdr, body := shopA.send(http.MethodGet, "/admin/analytics/export/revenue", nil)
		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, "text/csv", hdr.Get("Content-Type"))
		assert.Contains(t, hdr.Get("Content-Disposition"), "revenue_")
		records, err := csv.NewReader(bytes.NewReader(body)).ReadAll()
		require.NoError(t, err)
		require.Len(t, records, 2)
		assert.Equal(t, "date", records[0][0])
		assert.Equal(t, []string{today, "ZAR", "2", "1"}, records[1][:4])
		assert.Equal(t, "46000", records[1][9])

		for _, q := range []string{"top_products", "funnel", "gateways"} {
			status, _, body := shopA.send(http.MethodGet, "/admin/analytics/export/"+q, nil)
			require.Equal(t, http.StatusOK, status, q)
			records, err := csv.NewReader(bytes.NewReader(body)).ReadAll()
			require.NoError(t, err, q)
			assert.Greater(t, len(records), 1, q)
		}
		assert.Equal(t, http.StatusNotFound, shopA.get("/admin/analytics/export/cohorts", nil))
	})

	t.Run("milestones are announced once", func(t *testing.T) {
		got := outbox(events.TypeAnalyticsMilestoneReachedV1)
		require.Len(t, got, 3)
		assert.Equal(t, "first_order", got[0]["milestone"])
		assert.Equal(t, o1.OrderID, got[0]["order_id"])
		assert.Equal(t, "order_count", got[1]["milestone"])
		assert.InDelta(t, 3, got[1]["threshold"], 0)
		assert.Equal(t, "revenue", got[2]["milestone"])
		assert.Equal(t, "ZAR", got[2]["currency"])
		assert.Equal(t, o3.OrderID, got[2]["order_id"])

		// Shop B's old order (a replay) announces nothing; its first recent
		// one is not its first order.
		old := order("YC-2001", uuid.NewString(), "", now.AddDate(0, 0, -3), 0, line(skuA, 1, 5_000))
		publish("shop-b", old)
		require.Eventually(t, func() bool { return recorded(old.OrderID) }, 10*time.Second, 50*time.Millisecond)
		recent := order("YC-2002", uuid.NewString(), "", now, 0, line(skuA, 1, 5_000))
		publish("shop-b", events.OrderCancelledV1{
			OrderID: recent.OrderID, OrderNumber: recent.OrderNumber, UserID: recent.UserID,
			Reason: "customer", CancelledAt: now,
		})
		time.Sleep(200 * time.Millisecond)
		publish("shop-b", recent)
		require.Eventually(t, func() bool {
			return count(`SELECT COUNT(*) FROM fact_order WHERE order_id = $1 AND status = 'cancelled'`, recent.OrderID) == 1
		}, 15*time.Second, 100*time.Millisecond, "a cancellation consumed before its order is retried")
		assert.Len(t, outbox(events.TypeAnalyticsMilestoneReachedV1), 3)
	})

	t.Run("shops see only their own figures", func(t *testing.T) {
		assert.Equal(t, http.StatusForbidden, customer.get("/admin/analytics/dashboard", nil))
		assert.Equal(t, http.StatusForbidden, shopA.get("/sysadmin/analytics/platform", nil))

		var b api.Dashboard
		require.Eventually(t, func() bool {
			b = api.Dashboard{}
			return shopB.get("/admin/analytics/dashboard", &b) == http.StatusOK && b.PendingDays == 0 &&
				len(b.Revenue) == 1 && b.Revenue[0].Cancelled == 1
		}, 10*time.Second, 100*time.Millisecond)
		assert.Equal(t, int64(5_000), b.Revenue[0].GrossMinor)
		assert.Empty(t, b.Gateways)

		var platform api.Dashboard
		require.Equal(t, http.StatusOK, sysadmin.get("/sysadmin/analytics/platform", &platform))
		require.Len(t, platform.Revenue, 1)
		assert.Equal(t, int64(56_000), platform.Revenue[0].GrossMinor)

		var cmp api.TenantComparisonList
		require.Equal(t, http.StatusOK, sysadmin.get("/sysadmin/analytics/tenants/comparison", &cmp))
		require.Len(t, cmp.Tenants, 2)
		assert.Equal(t, "Shop A", cmp.Tenants[0].Name)
		assert.Equal(t, int64(51_000), cmp.Tenants[0].Revenue[0].GrossMinor)
		assert.Equal(t, int64(3), cmp.Tenants[0].Funnel.Visitors)
		assert.Equal(t, "Shop B", cmp.Tenants[1].Name)
	})

	t.Run("aggregates match the facts and rebuild from them", func(t *testing.T) {
		from := now.AddDate(0, 0, -7).Format(time.DateOnly)
		var c api.Consistency
		require.Equal(t, http.StatusOK, sysadmin.get("/sysadmin/analytics/consistency?from="+from, &c))
		assert.True(t, c.Consistent, "mismatches: %+v", c.Mismatches)

		require.NoError(t, dbkit.WithPlatformTx(ctx, db, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE agg_daily_revenue SET gross_minor = 1 WHERE tenant_id = 'shop-a'`)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `DELETE FROM agg_daily_funnel WHERE tenant_id = 'shop-b'`)
			return err
		}))
		require.Equal(t, http.StatusOK, sysadmin.get("/sysadmin/analytics/consistency?from="+from, &c))
		assert.False(t, c.Consistent)
		assert.ElementsMatch(t, []string{"agg_daily_revenue/shop-a", "agg_daily_funnel/shop-b", "agg_daily_funnel/shop-b"},
			func() []string {
				out := make([]string, 0, len(c.Mismatches))
				for _, m := range c.Mismatches {
					out = append(out, m.Aggregate+"/"+m.TenantID)
				}
				return out
			}(), "shop-b's funnel is missing for today and three days ago")

		var res api.RebuildResult
		require.Equal(t, http.StatusOK, sysadmin.do(http.MethodPost, "/sysadmin/analytics/rebuild",
			map[string]any{"from": from, "to": today}, &res))
		assert.Positive(t, res.Rows)
		assert.Len(t, res.Aggregates, 4)
		require.Equal(t, http.StatusOK, sysadmin.get("/sysadmin/analytics/consistency?from="+from, &c))
		assert.True(t, c.Consistent, "mismatches: %+v", c.Mismatches)

		rebuilt := outbox(events.TypeAnalyticsAggregateRebuiltV1)
		require.Len(t, rebuilt, 1)
		assert.Equal(t, "manual", rebuilt[0]["trigger"])
		assert.Equal(t, from, rebuilt[0]["from"])

		nightly, err := a.Service.Nightly(ctx)
		require.NoError(t, err)
		assert.Equal(t, 2, nightly.Range.Days())
		assert.Equal(t, "schedule", outbox(events.TypeAnalyticsAggregateRebuiltV1)[1]["trigger"])

		assert.Equal(t, http.StatusForbidden, shopA.do(http.MethodPost, "/sysadmin/analytics/rebuild",
			map[string]any{"from": from, "to": today}, nil))
		assert.Equal(t, http.StatusBadRequest, sysadmin.do(http.MethodPost, "/sysadmin/analytics/rebuild",
			map[string]any{"from": from, "to": today, "tenant_id": "_platform"}, nil))
	})

	t.Run("a deleted user's orders are pseudonymised", func(t *testing.T) {
		pseudonym := uuid.NewString()
		publish("shop-a", events.IdentityUserDeletedV1{
			UserID: buyer, Pseudonym: pseudonym, Role: "customer", RequestedBy: "user", DeletedAt: now,
		})
		require.Eventually(t, func() bool {
			return len(outbox(events.TypePlatformGdprCascadeCompletedV1)) == 1
		}, 10*time.Second, 50*time.Millisecond)
		done := outbox(events.TypePlatformGdprCascadeCompletedV1)[0]
		assert.Equal(t, "yc-analytics", done["service"])
		assert.Equal(t, "pseudonymized", done["action"])
		assert.InDelta(t, 1, done["record_count"], 0)
		assert.Zero(t, count(`SELECT COUNT(*) FROM fact_order WHERE user_id = $1`, buyer))
		assert.Equal(t, 1, count(`SELECT COUNT(*) FROM fact_order WHERE user_id = $1 AND order_id = $2`, pseudonym, o1.OrderID))
	})

	t.Run("readyz", func(t *testing.T) {
		resp, err := srv.Client().Get(srv.URL + "/readyz")
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})
}
