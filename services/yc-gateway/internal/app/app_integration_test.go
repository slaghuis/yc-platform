//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	"github.com/slaghuis/yc-platform/services/yc-gateway/internal/app"
)

// fakeIdentity issues "tok-<tenant>" visitors and "svc-<name>" service
// tokens, validates them and echoes the identity headers on /me. Its
// internal endpoints insist on the gateway's caller header, as
// authctx.Audience does.
func fakeIdentity(t *testing.T) string {
	t.Helper()
	expires := time.Now().Add(time.Hour).UTC()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/internal/") && r.Header.Get(authctx.HeaderCallerService) != "yc-gateway" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/internal/service-token":
			var req struct {
				ClientID     string `json:"client_id"`
				ClientSecret string `json:"client_secret"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.ClientSecret != "secret" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "svc-" + req.ClientID, "token_type": "Bearer", "expires_at": expires,
			})
			return
		}
		switch r.URL.Path {
		case "/visitor":
			tenant := r.Header.Get(authctx.HeaderTenantID)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "tok-" + tenant, "token_type": "Bearer",
				"expires_at": expires, "user_id": "visitor-" + tenant, "tenant_id": tenant,
			})
		case "/internal/validate-token":
			var req struct{ Token string }
			_ = json.NewDecoder(r.Body).Decode(&req)
			if name, ok := strings.CutPrefix(req.Token, "svc-"); ok {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"tenant_id": "_platform", "actor_id": name, "actor_type": "service",
					"jti": "jti-svc", "expires_at": expires,
				})
				return
			}
			tenant, ok := strings.CutPrefix(req.Token, "tok-")
			if !ok {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tenant_id": tenant, "actor_id": "visitor-" + tenant, "actor_type": "visitor",
				"jti": "jti", "expires_at": expires,
			})
		case "/me":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"tenant":        r.Header.Get(authctx.HeaderTenantID),
				"actor":         r.Header.Get(authctx.HeaderActorID),
				"authorization": r.Header.Get("Authorization"),
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

type result struct {
	StatusCode int
	Header     http.Header
	Cookies    []*http.Cookie
}

func get(t *testing.T, srv *httptest.Server, host, path string, out any) result {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+path, nil)
	require.NoError(t, err)
	req.Host = host
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	if out != nil && resp.StatusCode < 300 {
		require.NoError(t, json.NewDecoder(resp.Body).Decode(out))
	}
	return result{StatusCode: resp.StatusCode, Header: resp.Header, Cookies: resp.Cookies()}
}

func post(t *testing.T, srv *httptest.Server, path string, body any) int {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+path, bytes.NewReader(b))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	return resp.StatusCode
}

func provision(t *testing.T, bus *eventbus.Client, tenant string, hosts ...string) {
	t.Helper()
	require.NoError(t, bus.PublishDirect(context.Background(), eventbus.Event{
		EventType: events.TypePlatformTenantProvisionedV1,
		TenantID:  eventbus.PlatformTenantID,
		Actor:     eventbus.Actor{Type: "service", ID: "yc-tenant-cli"},
		Payload: events.PlatformTenantProvisionedV1{
			TenantID: tenant, Name: tenant, Hostnames: hosts,
			StoreCurrency: "ZAR", DefaultJurisdiction: "ZA", IsTaxInclusive: true,
			ProvisionedAt: time.Now().UTC(),
		},
	}))
}

func TestGateway(t *testing.T) {
	ctx := context.Background()
	telemetry.InitForTest(t, "yc-gateway")

	dbURL := testkit.Postgres(t)
	natsURL := testkit.NATS(t)
	t.Setenv("YC_GATEWAY_DB_URL", dbURL)
	t.Setenv("NATS_URL", natsURL)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("YC_GATEWAY__UPSTREAMS__IDENTITY", fakeIdentity(t))
	t.Setenv("YC_GATEWAY_COOKIE_SECURE", "false")
	t.Setenv("YC_GATEWAY__REGISTRY__HEALTH_INTERVAL", "100ms")
	cfg, err := app.LoadConfig()
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())

	a, err := app.New(ctx, cfg, httpkit.BuildInfo{Service: "yc-gateway", Version: "test"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	srv := httptest.NewServer(a.Handler)
	t.Cleanup(srv.Close)

	validator, err := events.NewValidator()
	require.NoError(t, err)
	cli, err := eventbus.NewClient(ctx, eventbus.Config{
		NATSURL: natsURL, ServiceName: "yc-tenant-cli", Validator: validator,
		Streams: []eventbus.StreamConfig{eventbus.PlatformStream()},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close(context.Background()) })

	t.Run("unknown host is 404", func(t *testing.T) {
		assert.Equal(t, http.StatusNotFound, get(t, srv, "shop-a.test", "/me", nil).StatusCode)
	})

	t.Run("provisioned tenant gets a visitor and is proxied", func(t *testing.T) {
		provision(t, cli, "shop-a", "shop-a.test", "www.shop-a.test")
		require.Eventually(t, func() bool {
			_, ok := a.Hosts.Resolve("www.shop-a.test")
			return ok
		}, 10*time.Second, 50*time.Millisecond)

		var me map[string]string
		resp := get(t, srv, "Shop-A.test:443", "/me", &me)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, map[string]string{
			"tenant": "shop-a", "actor": "visitor-shop-a", "authorization": "Bearer tok-shop-a",
		}, me)
		require.Len(t, resp.Cookies, 1)
		assert.Equal(t, "yc_visitor", resp.Cookies[0].Name)
		assert.NotEmpty(t, resp.Header.Get(authctx.HeaderCorrelationID))
	})

	t.Run("hostnames are durable and loaded by new instances", func(t *testing.T) {
		db, err := pgxpool.New(ctx, dbURL)
		require.NoError(t, err)
		defer db.Close()
		var n int
		require.NoError(t, db.QueryRow(ctx, `SELECT count(*) FROM tenant_hosts WHERE tenant_id = 'shop-a'`).Scan(&n))
		assert.Equal(t, 2, n)

		b, err := app.New(ctx, cfg, httpkit.BuildInfo{Service: "yc-gateway", Version: "test"})
		require.NoError(t, err)
		defer func() { _ = b.Close(context.Background()) }()
		tenant, ok := b.Hosts.Resolve("shop-a.test")
		assert.True(t, ok)
		assert.Equal(t, "shop-a", tenant)
	})

	t.Run("hostname owned by another tenant is dead-lettered", func(t *testing.T) {
		provision(t, cli, "shop-b", "shop-b.test", "shop-a.test")

		nc, err := nats.Connect(natsURL)
		require.NoError(t, err)
		defer nc.Close()
		js, err := jetstream.New(nc)
		require.NoError(t, err)
		stream, err := js.Stream(ctx, "YC_DLQ")
		require.NoError(t, err)
		var msg *jetstream.RawStreamMsg
		require.Eventually(t, func() bool {
			msg, err = stream.GetLastMsgForSubject(ctx, "yc._platform.platform.dlq.v1")
			return err == nil
		}, 10*time.Second, 50*time.Millisecond)
		var dlq eventbus.DLQEnvelope
		require.NoError(t, json.Unmarshal(msg.Data, &dlq))
		assert.Equal(t, "yc-gateway-tenant-provisioned", dlq.DLQMetadata.Consumer)
		assert.Contains(t, dlq.DLQMetadata.LastError, "already belongs to shop-a")

		tenant, _ := a.Hosts.Resolve("shop-a.test")
		assert.Equal(t, "shop-a", tenant)
		_, ok := a.Hosts.Resolve("shop-b.test")
		assert.False(t, ok, "the whole event is rejected")
	})

	t.Run("registered service is routed, then deregistered when unhealthy", func(t *testing.T) {
		var healthy atomic.Bool
		healthy.Store(true)
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/healthz" {
				if !healthy.Load() {
					w.WriteHeader(http.StatusServiceUnavailable)
				}
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"path": r.URL.Path, "tenant": r.Header.Get(authctx.HeaderTenantID)})
		}))
		defer backend.Close()
		internal := httptest.NewServer(a.InternalHandler)
		defer internal.Close()

		reg := httpkit.Registration{
			ServiceName: "yc-product", InstanceID: "product-1", BaseURL: backend.URL, HealthURL: "/healthz",
			Routes: []httpkit.RegisteredRoute{{Prefix: "/products", Methods: []string{"GET"}}},
		}
		require.Equal(t, http.StatusNoContent, post(t, internal, "/internal/register", reg))
		require.Equal(t, http.StatusNoContent, post(t, internal, "/internal/register", reg), "re-registration is idempotent")

		static := reg
		static.ServiceName, static.Routes = "yc-other", []httpkit.RegisteredRoute{{Prefix: "/me"}}
		assert.Equal(t, http.StatusBadRequest, post(t, internal, "/internal/register", static), "static prefix")
		taken := reg
		taken.ServiceName = "yc-other"
		assert.Equal(t, http.StatusConflict, post(t, internal, "/internal/register", taken), "another service's prefix")

		var got map[string]string
		require.Equal(t, http.StatusOK, get(t, srv, "shop-a.test", "/products/42", &got).StatusCode)
		assert.Equal(t, map[string]string{"path": "/products/42", "tenant": "shop-a"}, got)

		var routes struct{ Routes []map[string]any }
		require.Equal(t, http.StatusOK, get(t, internal, "x", "/internal/routes", &routes).StatusCode)
		assert.Contains(t, routes.Routes, map[string]any{
			"prefix": "/products", "methods": []any{"GET"}, "upstream": backend.URL, "service": "yc-product",
		})

		healthy.Store(false)
		require.Eventually(t, func() bool {
			return get(t, srv, "shop-a.test", "/products/42", nil).StatusCode == http.StatusNotFound
		}, 10*time.Second, 50*time.Millisecond)

		db, err := pgxpool.New(ctx, dbURL)
		require.NoError(t, err)
		defer db.Close()
		var types []string
		rows, err := db.Query(ctx, `SELECT envelope->>'event_type' FROM outbox
			WHERE envelope->>'event_type' LIKE 'platform.service.%' ORDER BY created_at`)
		require.NoError(t, err)
		for rows.Next() {
			var s string
			require.NoError(t, rows.Scan(&s))
			types = append(types, s)
		}
		require.NoError(t, rows.Err())
		assert.Equal(t, []string{"platform.service.registered.v1", "platform.service.deregistered.v1"}, types)

		assert.Equal(t, http.StatusNotFound, post(t, internal, "/internal/deregister",
			httpkit.Deregistration{ServiceName: "yc-product", InstanceID: "product-1"}), "already gone")
	})

	t.Run("internal paths are not served publicly", func(t *testing.T) {
		assert.Equal(t, http.StatusNotFound, get(t, srv, "shop-a.test", "/internal/routes", nil).StatusCode)
	})

	t.Run("service calls reach registered internal routes", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]string{
				"path":       r.URL.Path,
				"tenant":     r.Header.Get(authctx.HeaderTenantID),
				"actor":      r.Header.Get(authctx.HeaderActorID),
				"actor_type": r.Header.Get(authctx.HeaderActorType),
				"caller":     r.Header.Get(authctx.HeaderCallerService),
				"auth":       r.Header.Get("Authorization"),
			})
		}))
		defer backend.Close()
		internal := httptest.NewServer(a.InternalHandler)
		defer internal.Close()

		reg := httpkit.Registration{
			ServiceName: "yc-pricing", InstanceID: "pricing-1", BaseURL: backend.URL, HealthURL: "/healthz",
			Routes: []httpkit.RegisteredRoute{
				{Prefix: "/prices", Methods: []string{"GET"}},
				{Prefix: "/internal/evaluate", Methods: []string{"POST"}},
			},
		}
		require.Equal(t, http.StatusNoContent, post(t, internal, "/internal/register", reg))
		bad := reg
		bad.ServiceName, bad.Routes = "yc-other", []httpkit.RegisteredRoute{{Prefix: "/internal/routes"}}
		assert.Equal(t, http.StatusBadRequest, post(t, internal, "/internal/register", bad), "gateway's own path")

		client := httpkit.NewInternalClient("yc-cart", httpkit.InternalConfig{GatewayURL: internal.URL, ClientSecret: "secret"})
		callCtx := authctx.WithAuth(ctx, authctx.AuthContext{
			TenantID: "shop-a", ActorID: "u1", ActorType: authctx.ActorCustomer, Token: "tok-shop-a",
		})
		var got map[string]string
		require.NoError(t, client.Do(callCtx, http.MethodPost, "/internal/evaluate", map[string]int{"x": 1}, &got))
		assert.Equal(t, map[string]string{
			"path": "/internal/evaluate", "tenant": "shop-a", "actor": "u1", "actor_type": "customer",
			"caller": "yc-cart", "auth": "",
		}, got)

		assert.Equal(t, http.StatusNotFound, get(t, srv, "shop-a.test", "/internal/evaluate", nil).StatusCode,
			"internal routes are not public")
		assert.Equal(t, http.StatusUnauthorized, post(t, internal, "/internal/evaluate", map[string]int{"x": 1}),
			"no service token")

		wrong := httpkit.NewInternalClient("yc-cart", httpkit.InternalConfig{GatewayURL: internal.URL, ClientSecret: "nope"})
		err := wrong.Do(callCtx, http.MethodPost, "/internal/evaluate", nil, nil)
		assert.Equal(t, http.StatusUnauthorized, httpkit.UpstreamStatus(err))
	})

	t.Run("health and readiness need no tenant", func(t *testing.T) {
		assert.Equal(t, http.StatusOK, get(t, srv, "anything", "/healthz", nil).StatusCode)
		var r httpkit.Readiness
		require.Equal(t, http.StatusOK, get(t, srv, "anything", "/readyz", &r).StatusCode)
		assert.Equal(t, map[string]string{"postgres": "ok", "nats": "ok"}, r.Checks)
	})
}
