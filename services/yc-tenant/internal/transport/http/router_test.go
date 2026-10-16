package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	"github.com/slaghuis/yc-platform/services/yc-tenant/internal/domain"
)

func TestPostsRequireAnIdempotencyKey(t *testing.T) {
	// The idempotency middleware answers before the store or the service is
	// touched, so neither is needed.
	srv := httptest.NewServer(NewRouter(nil, nil, httpkit.BuildInfo{Service: "yc-tenant"}, nil))
	t.Cleanup(srv.Close)

	for _, path := range []string{
		"/sysadmin/tenants", "/sysadmin/tenants/acme/suspend", "/sysadmin/tenants/acme/reinstate",
	} {
		t.Run(path, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(`{}`))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(authctx.HeaderTenantID, "_platform")
			req.Header.Set(authctx.HeaderActorID, "admin-1")
			req.Header.Set(authctx.HeaderActorType, string(authctx.ActorSystemAdmin))
			resp, err := srv.Client().Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		})
	}
}

func TestRoutesAreSystemAdminOnly(t *testing.T) {
	routes := Routes()
	require.Len(t, routes, 1)
	assert.Equal(t, "/sysadmin/tenants", routes[0].Prefix)
	assert.Equal(t, []string{"system_admin"}, routes[0].Roles)
	assert.False(t, routes[0].Anonymous)
}

func TestTenantMapping(t *testing.T) {
	at := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	active := tenant(domain.Tenant{
		TenantID: "acme", Name: "Acme", Status: domain.StatusActive, StoreCurrency: "ZAR", DefaultJurisdiction: "ZA",
		ContactEmail: "o@acme.test", CreatedAt: at, UpdatedAt: at,
		Hostnames: []domain.Hostname{{Name: "acme.test", IsPrimary: true}, {Name: "www.acme.test"}},
	})
	assert.Equal(t, []string{"acme.test", "www.acme.test"}, active.Hostnames)
	require.NotNil(t, active.PrimaryHostname)
	assert.Equal(t, "acme.test", *active.PrimaryHostname)
	assert.Nil(t, active.SuspendedAt)
	assert.Nil(t, active.SuspendedReason)

	deleted := tenant(domain.Tenant{TenantID: "acme", Status: domain.StatusDeleted, DeletedAt: &at})
	assert.NotNil(t, deleted.Hostnames, "a deleted tenant lists no hostnames, as [] not null")
	assert.Empty(t, deleted.Hostnames)
	assert.Nil(t, deleted.PrimaryHostname)
	assert.Equal(t, &at, deleted.DeletedAt)
}
