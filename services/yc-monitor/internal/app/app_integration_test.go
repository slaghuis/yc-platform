//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	testkit "github.com/slaghuis/yc-platform/libs/yc-testkit"
	api "github.com/slaghuis/yc-platform/services/yc-monitor/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-monitor/internal/app"
	"github.com/slaghuis/yc-platform/services/yc-monitor/internal/service"
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

func (c client) do(method, path string, body, out any) int {
	c.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(c.t, json.NewEncoder(&buf).Encode(body))
	}
	req, err := http.NewRequestWithContext(context.Background(), method, c.srv.URL+path, &buf)
	require.NoError(c.t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range c.hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.srv.Client().Do(req)
	require.NoError(c.t, err)
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		c.t.Logf("%s %s: %d %s", method, path, resp.StatusCode, b)
	} else if out != nil {
		require.NoError(c.t, json.NewDecoder(resp.Body).Decode(out))
	}
	return resp.StatusCode
}

func (c client) get(path string, out any) int { return c.do(http.MethodGet, path, nil, out) }

// flaky is a service whose /readyz can be made to fail.
type flaky struct {
	*httptest.Server
	down atomic.Bool
}

func newFlaky(t *testing.T) *flaky {
	f := &flaky{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" || f.down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(f.Close)
	return f
}

// webhook records what is posted to it.
type webhook struct {
	*httptest.Server
	mu    sync.Mutex
	posts []service.WebhookAlert
}

func newWebhook(t *testing.T) *webhook {
	h := &webhook{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var a service.WebhookAlert
		_ = json.NewDecoder(r.Body).Decode(&a)
		h.mu.Lock()
		h.posts = append(h.posts, a)
		h.mu.Unlock()
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *webhook) rules() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.posts))
	for i, p := range h.posts {
		out[i] = p.Rule
	}
	return out
}

func TestMonitor(t *testing.T) {
	ctx := context.Background()
	logs := telemetry.InitForTest(t, "yc-monitor")
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	})

	natsURL := testkit.NATS(t)
	dbURL := testkit.Postgres(t)
	hook := newWebhook(t)
	t.Setenv("YC_MONITOR_DB_URL", dbURL)
	t.Setenv("NATS_URL", natsURL)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("YC_MONITOR_HEALTH_INTERVAL", "1h")
	t.Setenv("YC_MONITOR_EVALUATE_INTERVAL", "1h")
	t.Setenv("YC_MONITOR_CRITICAL_WEBHOOK", hook.URL)
	cfg, err := app.LoadConfig()
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())

	a, err := app.New(ctx, cfg, httpkit.BuildInfo{Service: "yc-monitor", Version: "test"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	srv := httptest.NewServer(a.Handler)
	t.Cleanup(srv.Close)

	db, err := pgxpool.New(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(db.Close)
	testkit.AssertTenantIsolation(t, db, "outbox")
	outbox := func(eventType string) []map[string]any {
		t.Helper()
		rows, err := db.Query(ctx, `SELECT envelope->'payload' || jsonb_build_object('_tenant', envelope->>'tenant_id')
			FROM outbox WHERE envelope->>'event_type' = $1 ORDER BY created_at, id`, eventType)
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
	opened := func(rule string) []map[string]any {
		t.Helper()
		var out []map[string]any
		for _, p := range outbox(events.TypeMonitorAlertOpenedV1) {
			if p["rule"] == rule {
				out = append(out, p)
			}
		}
		return out
	}

	validator, err := events.NewValidator()
	require.NoError(t, err)
	upstream, err := eventbus.NewClient(ctx, eventbus.Config{
		NATSURL: natsURL, ServiceName: "yc-upstream", Validator: validator,
		Streams: []eventbus.StreamConfig{
			eventbus.IdentityStream(), eventbus.PlatformStream(), eventbus.CheckoutStream(), eventbus.OrderStream(),
			eventbus.AuditStream(),
		},
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

	sysadmin := as(t, srv, "_platform", "system_admin")
	shopA := as(t, srv, "shop-a", "shop_admin")
	shopB := as(t, srv, "shop-b", "shop_admin")

	t.Run("visitor.created is recorded", func(t *testing.T) {
		userID, corr := uuid.NewString(), uuid.NewString()
		require.NoError(t, upstream.PublishDirect(ctx, eventbus.Event{
			EventType: events.TypeIdentityVisitorCreatedV1, TenantID: "shop-a",
			Actor: eventbus.Actor{Type: "visitor", ID: userID}, CorrelationID: corr,
			Payload: events.IdentityVisitorCreatedV1{
				UserID: userID, CreatedAt: time.Now().UTC(), TokenExpiresAt: time.Now().Add(time.Hour).UTC(),
			},
		}))
		var got api.ReceivedEventList
		require.Eventually(t, func() bool {
			got = api.ReceivedEventList{}
			return sysadmin.get("/sysadmin/events?tenant_id=shop-a", &got) == http.StatusOK && len(got.Events) == 1
		}, 10*time.Second, 50*time.Millisecond)
		assert.Equal(t, events.TypeIdentityVisitorCreatedV1, got.Events[0].EventType)
		assert.Equal(t, corr, *got.Events[0].CorrelationID)
	})

	var deadLetter api.DeadLetter
	t.Run("an invalid event is dead-lettered and counted", func(t *testing.T) {
		env := eventbus.Envelope{
			EventID: uuid.NewString(), EventType: events.TypeIdentityVisitorCreatedV1, SchemaVersion: 1,
			EventTime: time.Now().UTC(), TenantID: "shop-c", Actor: eventbus.Actor{Type: "visitor", ID: "x"},
			CorrelationID: uuid.NewString(), SourceService: "yc-identity",
			Payload: json.RawMessage(`{"user_id":"not-a-uuid"}`),
		}
		raw, err := json.Marshal(env)
		require.NoError(t, err)
		nc, err := nats.Connect(natsURL)
		require.NoError(t, err)
		defer nc.Close()
		js, err := jetstream.New(nc)
		require.NoError(t, err)
		_, err = js.Publish(ctx, "yc.shop-c."+events.TypeIdentityVisitorCreatedV1, raw)
		require.NoError(t, err)

		var got api.DeadLetterList
		require.Eventually(t, func() bool {
			got = api.DeadLetterList{}
			return sysadmin.get("/sysadmin/dlq?tenant_id=shop-c", &got) == http.StatusOK && len(got.Entries) == 1
		}, 10*time.Second, 50*time.Millisecond)
		deadLetter = got.Entries[0]
		assert.Equal(t, env.EventID, deadLetter.EventID)
		assert.Equal(t, "yc-monitor-visitor-created", deadLetter.Consumer)
		assert.Equal(t, "poison", deadLetter.FailureReason)
		assert.False(t, deadLetter.Acknowledged)

		var series api.MetricSeries
		require.Equal(t, http.StatusOK, sysadmin.get("/sysadmin/metrics?metric=events.dlq&bucket=1m", &series))
		require.Len(t, series.Points, 1)
		assert.EqualValues(t, 1, series.Points[0].Value)
		var summary api.DeadLetterSummary
		require.Equal(t, http.StatusOK, sysadmin.get("/sysadmin/dlq/summary", &summary))
		require.Len(t, summary.Items, 1)
		assert.Equal(t, "shop-c", summary.Items[0].TenantID)
	})

	t.Run("a dead-letter spike raises an alert", func(t *testing.T) {
		var th api.Thresholds
		require.Equal(t, http.StatusOK, sysadmin.do("PUT", "/sysadmin/thresholds",
			map[string]any{"rule": "dlq_rate", "threshold": 0.5}, &th))
		assert.Equal(t, "_platform", th.TenantID)
		assert.Equal(t, 1, th.WindowMinutes, "unset fields keep the default")
		assert.Equal(t, http.StatusBadRequest, sysadmin.do("PUT", "/sysadmin/thresholds",
			map[string]any{"rule": "dlq_rate", "tenant_id": "shop-a", "threshold": 1}, nil), "platform-wide rule")
		assert.Equal(t, http.StatusForbidden, shopA.do("PUT", "/sysadmin/thresholds",
			map[string]any{"rule": "dlq_rate", "threshold": 1}, nil))

		require.NoError(t, a.Service.Evaluate(ctx))
		require.NoError(t, a.Service.Evaluate(ctx))
		spikes := opened("dlq_rate")
		require.Len(t, spikes, 1, "one alert while it lasts")
		assert.Equal(t, "_platform", spikes[0]["_tenant"])
		assert.Equal(t, []any{"system_admin"}, spikes[0]["notify"])

		var alerts api.AlertList
		require.Equal(t, http.StatusOK, sysadmin.get("/sysadmin/alerts?rule=dlq_rate", &alerts))
		require.Len(t, alerts.Items, 1)
		alert := alerts.Items[0]
		var acked api.Alert
		require.Equal(t, http.StatusOK, sysadmin.do("POST", "/sysadmin/alerts/"+alert.ID.String()+"/ack", nil, &acked))
		assert.Equal(t, api.AlertStatus("acknowledged"), acked.Status, "a threshold alert stays until it clears")

		var dl api.DeadLetter
		path := "/sysadmin/dlq/" + strconv.FormatInt(deadLetter.DlqSequence, 10) + "/ack"
		require.Equal(t, http.StatusOK, sysadmin.do("POST", path, nil, &dl))
		assert.True(t, dl.Acknowledged)
		assert.Equal(t, http.StatusNotFound, sysadmin.do("POST", "/sysadmin/dlq/999999/ack", nil, nil))

		require.Equal(t, http.StatusOK, sysadmin.do("PUT", "/sysadmin/thresholds",
			map[string]any{"rule": "dlq_rate", "threshold": 10}, nil))
		require.NoError(t, a.Service.Evaluate(ctx))
		require.Len(t, outbox(events.TypeMonitorAlertResolvedV1), 1)
		require.Equal(t, http.StatusOK, sysadmin.get("/sysadmin/alerts?rule=dlq_rate", &alerts))
		assert.Equal(t, api.AlertStatus("resolved"), alerts.Items[0].Status)
	})

	t.Run("an unhealthy service alerts and recovers", func(t *testing.T) {
		pricing, catalog := newFlaky(t), newFlaky(t)
		now := time.Now().UTC()
		for name, s := range map[string]*flaky{"yc-pricing": pricing, "yc-product": catalog} {
			publish("_platform", events.PlatformServiceRegisteredV1{
				ServiceName: name, InstanceID: name + "-1", BaseURL: s.URL, RegisteredAt: now,
			})
		}
		require.Eventually(t, func() bool {
			var list api.ServiceList
			return sysadmin.get("/sysadmin/services", &list) == http.StatusOK && len(list.Items) == 2
		}, 10*time.Second, 50*time.Millisecond)

		require.NoError(t, a.Service.CheckHealth(ctx))
		var dash api.Dashboard
		require.Equal(t, http.StatusOK, sysadmin.get("/sysadmin/dashboard", &dash))
		assert.Equal(t, api.DashboardStatus("green"), dash.Status)
		assert.Equal(t, 2, dash.Services.Healthy)
		assert.Equal(t, http.StatusForbidden, shopA.get("/sysadmin/dashboard", nil))

		pricing.down.Store(true)
		for range 3 {
			require.NoError(t, a.Service.CheckHealth(ctx))
		}
		unhealthy := outbox(events.TypeMonitorServiceUnhealthyV1)
		require.Len(t, unhealthy, 1)
		assert.Equal(t, "yc-pricing", unhealthy[0]["service_name"])
		down := opened("service_unhealthy")
		require.Len(t, down, 1)
		assert.Equal(t, "critical", down[0]["severity"])
		assert.Equal(t, map[string]any{"service": "yc-pricing"}, down[0]["dimensions"])
		require.Equal(t, http.StatusOK, sysadmin.get("/sysadmin/dashboard", &dash))
		assert.Equal(t, api.DashboardStatus("red"), dash.Status)
		assert.Equal(t, 1, dash.Services.Unhealthy)
		assert.Equal(t, 1, dash.OpenAlerts.Critical)
		require.Eventually(t, func() bool { return assert.ObjectsAreEqual([]string{"service_unhealthy"}, hook.rules()) },
			10*time.Second, 50*time.Millisecond, "critical alerts go to the webhook")

		pricing.down.Store(false)
		for range 3 {
			require.NoError(t, a.Service.CheckHealth(ctx))
		}
		require.Len(t, outbox(events.TypeMonitorServiceRecoveredV1), 1)
		require.Equal(t, http.StatusOK, sysadmin.get("/sysadmin/dashboard", &dash))
		assert.Equal(t, api.DashboardStatus("green"), dash.Status)

		publish("_platform", events.PlatformServiceDeregisteredV1{
			ServiceName: "yc-product", InstanceID: "yc-product-1", Reason: "shutdown", DeregisteredAt: time.Now().UTC(),
		})
		require.Eventually(t, func() bool {
			var list api.ServiceList
			return sysadmin.get("/sysadmin/services", &list) == http.StatusOK && list.Items[1].Status == "deregistered"
		}, 10*time.Second, 50*time.Millisecond)
		catalog.down.Store(true)
		for range 3 {
			require.NoError(t, a.Service.CheckHealth(ctx))
		}
		assert.Len(t, outbox(events.TypeMonitorServiceUnhealthyV1), 1, "a service that shut down is not checked")
	})

	t.Run("a falling checkout success rate alerts the shop", func(t *testing.T) {
		require.Equal(t, http.StatusOK, sysadmin.do("PUT", "/sysadmin/thresholds",
			map[string]any{"rule": "checkout_success_rate", "tenant_id": "shop-a", "min_volume": 3}, nil))
		var list api.ThresholdList
		require.Equal(t, http.StatusOK, sysadmin.get("/sysadmin/thresholds", &list))
		require.Len(t, list.Items, 3, "two platform rules and the shop's override")

		completed := func() {
			publish("shop-a", events.CheckoutCompletedV1{
				SessionID: uuid.NewString(), UserID: uuid.NewString(), CartType: "user", OrderID: uuid.NewString(),
				OrderNumber: "YC-1", Items: []events.CheckoutCompletedV1ItemsItem{{SKUID: uuid.NewString(), Quantity: 1}},
				Total: events.Money{AmountMinor: 1000, Currency: "ZAR"}, CompletedAt: time.Now().UTC(),
			})
		}
		completed()
		for range 3 {
			publish("shop-a", events.CheckoutFailedV1{
				SessionID: uuid.NewString(), UserID: uuid.NewString(), FailedState: "AUTHORIZING",
				Reason: "payment_declined", FailedAt: time.Now().UTC(),
			})
		}
		var failed api.MetricSeries
		require.Eventually(t, func() bool {
			failed = api.MetricSeries{}
			return shopA.get("/admin/metrics?metric=checkouts.failed&bucket=1m", &failed) == http.StatusOK &&
				len(failed.Points) == 1 && failed.Points[0].Value == 3
		}, 10*time.Second, 50*time.Millisecond)
		var none api.MetricSeries
		require.Equal(t, http.StatusOK, shopB.get("/admin/metrics?metric=checkouts.failed&bucket=1m", &none))
		assert.Empty(t, none.Points)
		require.Eventually(t, func() bool {
			var done api.MetricSeries
			return shopA.get("/admin/metrics?metric=checkouts.completed&bucket=1m", &done) == http.StatusOK &&
				len(done.Points) == 1
		}, 10*time.Second, 50*time.Millisecond)

		require.NoError(t, a.Service.Evaluate(ctx))
		low := opened("checkout_success_rate")
		require.Len(t, low, 1)
		assert.Equal(t, "shop-a", low[0]["_tenant"])
		assert.Equal(t, []any{"system_admin", "shop_admin"}, low[0]["notify"])
		assert.InDelta(t, 0.25, low[0]["value"], 0.001)

		var mine, theirs api.AlertList
		require.Equal(t, http.StatusOK, shopA.get("/admin/alerts?status=active", &mine))
		require.Len(t, mine.Items, 1)
		require.Equal(t, http.StatusOK, shopB.get("/admin/alerts", &theirs))
		assert.Empty(t, theirs.Items)
		assert.Equal(t, http.StatusNotFound, shopB.do("POST", "/admin/alerts/"+mine.Items[0].ID.String()+"/ack", nil, nil))

		for range 3 {
			completed()
		}
		require.Eventually(t, func() bool {
			var done api.MetricSeries
			return shopA.get("/admin/metrics?metric=checkouts.completed&bucket=1m", &done) == http.StatusOK &&
				len(done.Points) == 1 && done.Points[0].Value == 4
		}, 10*time.Second, 50*time.Millisecond)
		require.NoError(t, a.Service.Evaluate(ctx))
		require.Equal(t, http.StatusOK, shopA.get("/admin/alerts?status=active", &mine))
		assert.Empty(t, mine.Items, "4 of 7 is above 50%")
	})

	t.Run("a chargeback alerts the shop's admins", func(t *testing.T) {
		orderID := uuid.NewString()
		publish("shop-a", events.OrderDisputedV1{
			OrderID: orderID, OrderNumber: "YC-1001", UserID: uuid.NewString(), ChargebackID: uuid.NewString(),
			Amount: events.Money{AmountMinor: 12_500, Currency: "ZAR"}, Reason: ptr("fraudulent"), DisputedAt: time.Now().UTC(),
		})
		require.Eventually(t, func() bool { return len(opened("order_disputed")) == 1 }, 10*time.Second, 50*time.Millisecond)
		disputed := opened("order_disputed")[0]
		assert.Equal(t, "shop-a", disputed["_tenant"])
		assert.Equal(t, []any{"shop_admin"}, disputed["notify"])
		assert.Equal(t, "Order YC-1001 is disputed: a chargeback of ZAR 125.00 (fraudulent)", disputed["summary"])

		var mine api.AlertList
		require.Equal(t, http.StatusOK, shopA.get("/admin/alerts?rule=order_disputed", &mine))
		require.Len(t, mine.Items, 1)
		var acked api.Alert
		require.Equal(t, http.StatusOK, shopA.do("POST", "/admin/alerts/"+mine.Items[0].ID.String()+"/ack", nil, &acked))
		assert.Equal(t, api.AlertStatus("resolved"), acked.Status, "acknowledging an incident resolves it")
		assert.Len(t, outbox(events.TypeMonitorAlertAcknowledgedV1), 2)
	})

	t.Run("a GDPR incident is a critical platform alert", func(t *testing.T) {
		publish("shop-a", events.AuditGdprCascadeIncidentV1{
			UserID: uuid.NewString(), Pseudonym: uuid.NewString(), MissingServices: []string{"yc-review"},
			InitiatedAt: time.Now().Add(-73 * time.Hour).UTC(), Deadline: time.Now().Add(-time.Hour).UTC(),
		})
		require.Eventually(t, func() bool { return len(opened("gdpr_cascade_incident")) == 1 }, 10*time.Second, 50*time.Millisecond)
		incident := opened("gdpr_cascade_incident")[0]
		assert.Equal(t, "_platform", incident["_tenant"])
		assert.Equal(t, "critical", incident["severity"])
		assert.Contains(t, incident["summary"], "yc-review")
		require.Eventually(t, func() bool {
			return assert.ObjectsAreEqual([]string{"service_unhealthy", "gdpr_cascade_incident"}, hook.rules())
		}, 10*time.Second, 50*time.Millisecond)
	})

	t.Run("endpoints need the right admin", func(t *testing.T) {
		assert.Equal(t, http.StatusUnauthorized, client{t: t, srv: srv}.get("/sysadmin/dlq", nil))
		assert.Equal(t, http.StatusForbidden, shopA.get("/sysadmin/dlq", nil))
		assert.Equal(t, http.StatusForbidden, shopA.get("/sysadmin/events", nil))
		assert.Equal(t, http.StatusForbidden, as(t, srv, "shop-a", "customer").get("/admin/alerts", nil))
		assert.Equal(t, http.StatusBadRequest, sysadmin.get("/sysadmin/events?limit=9999", nil))
		assert.Equal(t, http.StatusBadRequest, sysadmin.get("/sysadmin/metrics?metric=events.dlq&bucket=2m", nil))
	})

	t.Run("readyz reports dependencies", func(t *testing.T) {
		var r httpkit.Readiness
		require.Equal(t, http.StatusOK, client{t: t, srv: srv}.get("/readyz", &r))
		assert.Equal(t, map[string]string{"postgres": "ok", "nats": "ok"}, r.Checks)
	})
}

func ptr[T any](v T) *T { return &v }
