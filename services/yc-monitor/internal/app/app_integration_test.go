//go:build integration

package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
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
	"github.com/slaghuis/yc-platform/services/yc-monitor/internal/app"
	transport "github.com/slaghuis/yc-platform/services/yc-monitor/internal/transport/http"
)

var sysadmin = map[string]string{
	authctx.HeaderTenantID:  "_platform",
	authctx.HeaderActorID:   "admin-1",
	authctx.HeaderActorType: "system_admin",
}

func get(t *testing.T, srv *httptest.Server, path string, hdr map[string]string, out any) int {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+path, nil)
	require.NoError(t, err)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	if out != nil && resp.StatusCode < 300 {
		require.NoError(t, json.NewDecoder(resp.Body).Decode(out))
	}
	return resp.StatusCode
}

func TestMonitor(t *testing.T) {
	ctx := context.Background()
	telemetry.InitForTest(t, "yc-monitor")

	natsURL := testkit.NATS(t)
	t.Setenv("YC_MONITOR_DB_URL", testkit.Postgres(t))
	t.Setenv("NATS_URL", natsURL)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	cfg, err := app.LoadConfig()
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())

	a, err := app.New(ctx, cfg, httpkit.BuildInfo{Service: "yc-monitor", Version: "test"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	srv := httptest.NewServer(a.Handler)
	t.Cleanup(srv.Close)

	validator, err := events.NewValidator()
	require.NoError(t, err)
	identity, err := eventbus.NewClient(ctx, eventbus.Config{
		NATSURL: natsURL, ServiceName: "yc-identity", Validator: validator,
		Streams: []eventbus.StreamConfig{eventbus.IdentityStream()},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = identity.Close(context.Background()) })

	t.Run("visitor.created is recorded", func(t *testing.T) {
		userID := uuid.NewString()
		corr := uuid.NewString()
		require.NoError(t, identity.PublishDirect(ctx, eventbus.Event{
			EventType:     events.TypeIdentityVisitorCreatedV1,
			TenantID:      "shop-a",
			Actor:         eventbus.Actor{Type: "visitor", ID: userID},
			CorrelationID: corr,
			Payload: events.IdentityVisitorCreatedV1{
				UserID: userID, CreatedAt: time.Now().UTC(), TokenExpiresAt: time.Now().Add(time.Hour).UTC(),
			},
		}))

		var got transport.EventsResponse
		require.Eventually(t, func() bool {
			got = transport.EventsResponse{}
			return get(t, srv, "/sysadmin/events?tenant_id=shop-a", sysadmin, &got) == http.StatusOK &&
				len(got.Events) == 1
		}, 10*time.Second, 50*time.Millisecond)
		e := got.Events[0]
		assert.Equal(t, events.TypeIdentityVisitorCreatedV1, e.EventType)
		assert.Equal(t, "yc-identity", e.SourceService)
		assert.Equal(t, corr, e.CorrelationID)

		var other transport.EventsResponse
		require.Equal(t, http.StatusOK, get(t, srv, "/sysadmin/events?tenant_id=shop-b", sysadmin, &other))
		assert.Empty(t, other.Events)
	})

	t.Run("invalid event is dead-lettered and recorded", func(t *testing.T) {
		env := eventbus.Envelope{
			EventID:       uuid.NewString(),
			EventType:     events.TypeIdentityVisitorCreatedV1,
			SchemaVersion: 1,
			EventTime:     time.Now().UTC(),
			TenantID:      "shop-c",
			Actor:         eventbus.Actor{Type: "visitor", ID: "x"},
			CorrelationID: uuid.NewString(),
			SourceService: "yc-identity",
			Payload:       json.RawMessage(`{"user_id":"not-a-uuid"}`),
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

		var got transport.DLQResponse
		require.Eventually(t, func() bool {
			got = transport.DLQResponse{}
			return get(t, srv, "/sysadmin/dlq?tenant_id=shop-c", sysadmin, &got) == http.StatusOK &&
				len(got.Entries) == 1
		}, 10*time.Second, 50*time.Millisecond)
		d := got.Entries[0]
		assert.Equal(t, env.EventID, d.EventID)
		assert.Equal(t, "yc-monitor", d.Service)
		assert.Equal(t, "yc-monitor-visitor-created", d.Consumer)
		assert.Equal(t, "poison", d.FailureReason)
		assert.NotEmpty(t, d.LastError)
		assert.NotZero(t, d.DLQSequence)

		var evts transport.EventsResponse
		require.Equal(t, http.StatusOK, get(t, srv, "/sysadmin/events?tenant_id=shop-c", sysadmin, &evts))
		assert.Empty(t, evts.Events, "the invalid event never reached the handler")
	})

	t.Run("sysadmin endpoints need a system admin", func(t *testing.T) {
		for _, path := range []string{"/sysadmin/events", "/sysadmin/dlq"} {
			assert.Equal(t, http.StatusUnauthorized, get(t, srv, path, nil, nil), path)
			assert.Equal(t, http.StatusForbidden, get(t, srv, path, map[string]string{
				authctx.HeaderTenantID: "shop-a", authctx.HeaderActorID: "u", authctx.HeaderActorType: "shop_admin",
			}, nil), path)
		}
		assert.Equal(t, http.StatusBadRequest,
			get(t, srv, "/sysadmin/events?limit="+url.QueryEscape("9999"), sysadmin, nil))
	})

	t.Run("readyz reports dependencies", func(t *testing.T) {
		var r httpkit.Readiness
		require.Equal(t, http.StatusOK, get(t, srv, "/readyz", nil, &r))
		assert.Equal(t, map[string]string{"postgres": "ok", "nats": "ok"}, r.Checks)
	})
}
