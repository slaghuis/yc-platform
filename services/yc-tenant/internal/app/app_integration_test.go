//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

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
	api "github.com/slaghuis/yc-platform/services/yc-tenant/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-tenant/internal/app"
)

type client struct {
	t   *testing.T
	srv *httptest.Server
	hdr map[string]string
}

func as(t *testing.T, srv *httptest.Server, actorType string) client {
	return client{t: t, srv: srv, hdr: map[string]string{
		authctx.HeaderTenantID:  eventbus.PlatformTenantID,
		authctx.HeaderActorID:   uuid.NewString(),
		authctx.HeaderActorType: actorType,
	}}
}

func (c client) do(method, path string, body, out any, headers map[string]string) int {
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
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.srv.Client().Do(req)
	require.NoError(c.t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(c.t, err)
	if resp.StatusCode >= 400 {
		c.t.Logf("%s %s: %d %s", method, path, resp.StatusCode, raw)
	} else if out != nil && len(raw) > 0 {
		require.NoError(c.t, json.Unmarshal(raw, out))
	}
	return resp.StatusCode
}

func TestTenantRegistry(t *testing.T) {
	ctx := context.Background()
	logs := telemetry.InitForTest(t, "yc-tenant")
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	})

	dbURL := testkit.Postgres(t)
	natsURL := testkit.NATS(t)
	t.Setenv("YC_TENANT_DB_URL", dbURL)
	t.Setenv("NATS_URL", natsURL)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("YC_TENANT_PLATFORM_HOSTS", "admin.test")
	cfg, err := app.LoadConfig()
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())
	a, err := app.New(ctx, cfg, httpkit.BuildInfo{Service: "yc-tenant", Version: "test"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	srv := httptest.NewServer(a.Handler)
	t.Cleanup(srv.Close)

	db, err := pgxpool.New(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(db.Close)

	admin := as(t, srv, string(authctx.ActorSystemAdmin))
	shop := as(t, srv, string(authctx.ActorShopAdmin))

	assert.Equal(t, http.StatusForbidden, shop.do(http.MethodGet, "/sysadmin/tenants", nil, nil, nil))

	var created api.Tenant
	body := api.CreateInput{
		TenantID: "acme", Name: "Acme", Hostnames: []string{"shop.acme.test"},
		StoreCurrency: "ZAR", DefaultJurisdiction: "ZA", IsTaxInclusive: true, ContactEmail: "ops@acme.test",
	}
	assert.Equal(t, http.StatusCreated, admin.do(http.MethodPost, "/sysadmin/tenants", body, &created, map[string]string{
		httpkit.HeaderIdempotencyKey: "create-1",
	}))
	assert.Equal(t, "acme", created.TenantID)
	assert.Equal(t, api.Active, created.Status)

	assert.Equal(t, http.StatusConflict, admin.do(http.MethodPost, "/sysadmin/tenants", api.CreateInput{
		TenantID: "other", Name: "Other", Hostnames: []string{"admin.test"},
		StoreCurrency: "ZAR", DefaultJurisdiction: "ZA", IsTaxInclusive: false, ContactEmail: "a@b.c",
	}, nil, map[string]string{httpkit.HeaderIdempotencyKey: "create-plat"}))

	var listed api.TenantList
	assert.Equal(t, http.StatusOK, admin.do(http.MethodGet, "/sysadmin/tenants?status=active", nil, &listed, nil))
	require.Len(t, listed.Items, 1)

	assert.Equal(t, http.StatusOK, admin.do(http.MethodPost, "/sysadmin/tenants/acme/suspend",
		api.SuspendInput{Reason: "non-payment"}, nil, map[string]string{httpkit.HeaderIdempotencyKey: "suspend-1"}))
	assert.Equal(t, http.StatusOK, admin.do(http.MethodPost, "/sysadmin/tenants/acme/reinstate",
		nil, nil, map[string]string{httpkit.HeaderIdempotencyKey: "reinstate-1"}))
	assert.Equal(t, http.StatusOK, admin.do(http.MethodPost, "/sysadmin/tenants/acme/suspend",
		api.SuspendInput{Reason: "closing"}, nil, map[string]string{httpkit.HeaderIdempotencyKey: "suspend-2"}))
	assert.Equal(t, http.StatusOK, admin.do(http.MethodDelete, "/sysadmin/tenants/acme",
		api.DeleteInput{ConfirmTenantID: "acme"}, &created, nil))
	assert.Equal(t, api.Deleted, created.Status)
	assert.Empty(t, created.Hostnames)

	var count int
	require.NoError(t, db.QueryRow(ctx, `SELECT COUNT(*) FROM outbox WHERE envelope->>'event_type' = $1`,
		events.TypePlatformTenantProvisionedV1).Scan(&count))
	assert.Equal(t, 1, count)
	require.NoError(t, db.QueryRow(ctx, `SELECT COUNT(*) FROM outbox WHERE envelope->>'event_type' = $1`,
		events.TypePlatformTenantDeletedV1).Scan(&count))
	assert.Equal(t, 1, count)
}
