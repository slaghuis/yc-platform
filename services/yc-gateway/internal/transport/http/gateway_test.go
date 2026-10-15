package http_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	"github.com/slaghuis/yc-platform/services/yc-gateway/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-gateway/internal/integration"
	transport "github.com/slaghuis/yc-platform/services/yc-gateway/internal/transport/http"
)

type hosts map[string]string

func (h hosts) Resolve(host string) (string, bool) {
	t, ok := h[host]
	return t, ok
}

// fakeIdentity knows tokens "<tenant>:<actor_type>"; it issues visitors as
// "<tenant>:visitor:<n>".
type fakeIdentity struct {
	mu      sync.Mutex
	issued  int
	failing bool
}

func (f *fakeIdentity) Validate(_ context.Context, token string) (domain.Identity, error) {
	if f.failing {
		return domain.Identity{}, ycerrors.UpstreamFailure("identity down")
	}
	tenant, actor, ok := cut(token)
	if !ok {
		return domain.Identity{}, integration.ErrInvalidToken
	}
	return domain.Identity{Token: token, TokenID: "jti-" + token, TenantID: tenant, ActorID: "user-" + token, ActorType: authctx.ActorType(actor)}, nil
}

func (f *fakeIdentity) CreateVisitor(_ context.Context, tenantID string) (domain.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issued++
	tok := tenantID + ":visitor"
	return domain.Identity{Token: tok, TenantID: tenantID, ActorID: "visitor-1", ActorType: authctx.ActorVisitor, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func cut(token string) (string, string, bool) {
	for i := range len(token) {
		if token[i] == ':' {
			return token[:i], token[i+1:], token[:i] != "" && token[i+1:] != ""
		}
	}
	return "", "", false
}

// upstreamSeen is what the backend received.
type upstreamSeen struct {
	Path, Auth, Tenant, Actor, ActorType, Correlation, Cookie, Forwarded string
}

func setup(t *testing.T) (http.Handler, *fakeIdentity, chan upstreamSeen) {
	t.Helper()
	return setupLimited(t, nil)
}

func setupLimited(t *testing.T, limiter transport.Limiter) (http.Handler, *fakeIdentity, chan upstreamSeen) {
	t.Helper()
	telemetry.InitForTest(t, "yc-gateway")

	seen := make(chan upstreamSeen, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- upstreamSeen{
			Path:        r.URL.Path,
			Auth:        r.Header.Get("Authorization"),
			Tenant:      r.Header.Get(authctx.HeaderTenantID),
			Actor:       r.Header.Get(authctx.HeaderActorID),
			ActorType:   r.Header.Get(authctx.HeaderActorType),
			Correlation: r.Header.Get(authctx.HeaderCorrelationID),
			Cookie:      r.Header.Get("Cookie"),
			Forwarded:   r.Header.Get("X-Forwarded-Host"),
		}
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(backend.Close)
	up, err := url.Parse(backend.URL)
	require.NoError(t, err)

	routes, err := domain.NewRoutes([]domain.Route{
		{Prefix: "/me", Methods: []string{"GET"}, Upstream: up},
		{Prefix: "/account", Upstream: up, Roles: []authctx.ActorType{authctx.ActorCustomer}},
		{Prefix: "/login", Upstream: up, Anonymous: true},
		{Prefix: "/webhooks/stripe", Methods: []string{"POST"}, Upstream: up, Callback: true},
	})
	require.NoError(t, err)

	id := &fakeIdentity{}
	gw := transport.New(hosts{"shop-a.test": "shop-a", "shop-b.test": "shop-b", "admin.test": "_platform"}, id, routes,
		limiter, transport.CookieConfig{Name: "yc_visitor"})
	return transport.NewRouter(gw, httpkit.BuildInfo{Service: "yc-gateway"}, nil), id, seen
}

func do(h http.Handler, method, host, path string, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	r.Host = host
	for _, m := range mutate {
		m(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func bearer(tok string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}

func cookie(tok string) func(*http.Request) {
	return func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: "yc_visitor", Value: tok})
		r.AddCookie(&http.Cookie{Name: "theme", Value: "dark"})
	}
}

func problemCode(t *testing.T, w *httptest.ResponseRecorder) ycerrors.Code {
	t.Helper()
	var p ycerrors.Problem
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &p))
	return p.Code
}

func TestAnonymousGetsVisitorCookie(t *testing.T) {
	h, id, seen := setup(t)
	const corrID = "0192f0c1-6a2b-7c3d-8e4f-5a6b7c8d9e0f"

	w := do(h, "GET", "shop-a.test", "/me", func(r *http.Request) {
		r.Header.Set(authctx.HeaderTenantID, "shop-b") // spoofed
		r.Header.Set(authctx.HeaderCorrelationID, corrID)
	})

	require.Equal(t, http.StatusTeapot, w.Code)
	got := <-seen
	assert.Equal(t, upstreamSeen{
		Path: "/me", Auth: "Bearer shop-a:visitor", Tenant: "shop-a", Actor: "visitor-1",
		ActorType: "visitor", Correlation: corrID, Forwarded: "shop-a.test",
	}, got)
	assert.Equal(t, corrID, w.Header().Get(authctx.HeaderCorrelationID))

	c := w.Result().Cookies()
	require.Len(t, c, 1)
	assert.Equal(t, "yc_visitor", c[0].Name)
	assert.Equal(t, "shop-a:visitor", c[0].Value)
	assert.True(t, c[0].HttpOnly)
	assert.Equal(t, http.SameSiteLaxMode, c[0].SameSite)
	assert.Equal(t, 1, id.issued)
}

func TestCallbackRoute(t *testing.T) {
	h, id, seen := setup(t)

	w := do(h, "POST", "shop-a.test", "/webhooks/stripe", func(r *http.Request) {
		r.Header.Set(authctx.HeaderActorID, "spoofed")
	})

	require.Equal(t, http.StatusTeapot, w.Code)
	got := <-seen
	assert.Equal(t, "shop-a", got.Tenant)
	assert.Empty(t, got.Actor)
	assert.Empty(t, got.Auth)
	assert.Empty(t, w.Result().Cookies(), "no visitor is issued")
	assert.Zero(t, id.issued)

	w = do(h, "POST", "unknown.test", "/webhooks/stripe")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestVisitorCookieReused(t *testing.T) {
	h, id, seen := setup(t)

	w := do(h, "GET", "shop-a.test", "/me", cookie("shop-a:visitor"))

	require.Equal(t, http.StatusTeapot, w.Code)
	got := <-seen
	assert.Equal(t, "Bearer shop-a:visitor", got.Auth)
	assert.Equal(t, "theme=dark", got.Cookie, "visitor cookie stripped, others kept")
	assert.Empty(t, w.Result().Cookies())
	assert.Zero(t, id.issued)
}

func TestStaleOrForeignCookieReplaced(t *testing.T) {
	for name, tok := range map[string]string{"invalid": "garbage", "other shop": "shop-b:visitor"} {
		t.Run(name, func(t *testing.T) {
			h, id, seen := setup(t)

			w := do(h, "GET", "shop-a.test", "/me", cookie(tok))

			require.Equal(t, http.StatusTeapot, w.Code)
			assert.Equal(t, "shop-a", (<-seen).Tenant)
			require.Len(t, w.Result().Cookies(), 1)
			assert.Equal(t, 1, id.issued)
		})
	}
}

func TestBearer(t *testing.T) {
	h, id, seen := setup(t)

	w := do(h, "GET", "shop-a.test", "/account/orders", bearer("shop-a:customer"))
	require.Equal(t, http.StatusTeapot, w.Code)
	got := <-seen
	assert.Equal(t, "/account/orders", got.Path)
	assert.Equal(t, "customer", got.ActorType)
	assert.Equal(t, "Bearer shop-a:customer", got.Auth)
	assert.Zero(t, id.issued)
}

func TestRejections(t *testing.T) {
	for _, tc := range []struct {
		name, host, method, path string
		mutate                   []func(*http.Request)
		status                   int
		code                     ycerrors.Code
	}{
		{"unknown host", "nope.test", "GET", "/me", nil, 404, ycerrors.CodeNotFound},
		{"no route", "shop-a.test", "GET", "/nothing", nil, 404, ycerrors.CodeNotFound},
		{"method not routed", "shop-a.test", "POST", "/me", nil, 404, ycerrors.CodeNotFound},
		{"internal never routed", "shop-a.test", "POST", "/internal/validate-token", nil, 404, ycerrors.CodeNotFound},
		{"invalid bearer", "shop-a.test", "GET", "/me", []func(*http.Request){bearer("garbage")}, 401, ycerrors.CodeUnauthorized},
		{"bearer for other shop", "shop-a.test", "GET", "/me", []func(*http.Request){bearer("shop-b:customer")}, 403, ycerrors.CodeForbidden},
		{"visitor on customer route", "shop-a.test", "GET", "/account", nil, 401, ycerrors.CodeUnauthorized},
		{"wrong role", "shop-a.test", "GET", "/account", []func(*http.Request){bearer("shop-a:shop_admin")}, 403, ycerrors.CodeForbidden},
		{"service token on the public listener", "admin.test", "GET", "/me", []func(*http.Request){bearer("_platform:service")}, 401, ycerrors.CodeUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, seen := setup(t)

			w := do(h, tc.method, tc.host, tc.path, tc.mutate...)

			assert.Equal(t, tc.status, w.Code)
			assert.Equal(t, tc.code, problemCode(t, w))
			assert.Empty(t, seen)
		})
	}
}

func TestPlatformHost(t *testing.T) {
	t.Run("anonymous route proceeds without an actor or visitor", func(t *testing.T) {
		h, id, seen := setup(t)

		w := do(h, "POST", "admin.test", "/login", cookie("shop-a:visitor"))

		require.Equal(t, http.StatusTeapot, w.Code)
		got := <-seen
		assert.Equal(t, "_platform", got.Tenant)
		assert.Empty(t, got.Actor)
		assert.Empty(t, got.ActorType)
		assert.Empty(t, got.Auth)
		assert.Empty(t, w.Result().Cookies())
		assert.Zero(t, id.issued)
	})

	t.Run("other routes need a token", func(t *testing.T) {
		h, id, seen := setup(t)

		w := do(h, "GET", "admin.test", "/me")

		assert.Equal(t, http.StatusUnauthorized, w.Code)
		assert.Empty(t, seen)
		assert.Zero(t, id.issued)
	})

	t.Run("system admin token is forwarded", func(t *testing.T) {
		h, _, seen := setup(t)

		w := do(h, "GET", "admin.test", "/me", bearer("_platform:system_admin"))

		require.Equal(t, http.StatusTeapot, w.Code)
		assert.Equal(t, "system_admin", (<-seen).ActorType)
	})

	t.Run("platform token on a shop host is refused", func(t *testing.T) {
		h, _, seen := setup(t)

		w := do(h, "GET", "shop-a.test", "/me", bearer("_platform:system_admin"))

		assert.Equal(t, http.StatusForbidden, w.Code)
		assert.Empty(t, seen)
	})
}

// recordingLimiter refuses actor keys in deny and records every call.
type recordingLimiter struct {
	mu    sync.Mutex
	calls []string
	deny  map[string]bool
}

func (l *recordingLimiter) Allow(tenantID string, actorType authctx.ActorType, actorKey string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, tenantID+"/"+string(actorType)+"/"+actorKey)
	if l.deny[actorKey] {
		return false, 1500 * time.Millisecond
	}
	return true, 0
}

func TestRateLimit(t *testing.T) {
	t.Run("signed-in actors by ID", func(t *testing.T) {
		l := &recordingLimiter{}
		h, _, seen := setupLimited(t, l)

		require.Equal(t, http.StatusTeapot, do(h, "GET", "shop-a.test", "/me", bearer("shop-a:customer")).Code)
		<-seen
		assert.Equal(t, []string{"shop-a/customer/user-shop-a:customer"}, l.calls)
	})

	t.Run("callers without credentials by address, before a visitor is issued", func(t *testing.T) {
		l := &recordingLimiter{deny: map[string]bool{"addr:192.0.2.1": true}}
		h, id, seen := setupLimited(t, l)

		w := do(h, "GET", "shop-a.test", "/me")

		assert.Equal(t, http.StatusTooManyRequests, w.Code)
		assert.Equal(t, ycerrors.CodeRateLimited, problemCode(t, w))
		assert.Equal(t, "2", w.Header().Get("Retry-After"))
		assert.Equal(t, []string{"shop-a/visitor/addr:192.0.2.1"}, l.calls)
		assert.Zero(t, id.issued)
		assert.Empty(t, seen)
	})

	t.Run("visitors with a cookie by visitor ID", func(t *testing.T) {
		l := &recordingLimiter{}
		h, _, seen := setupLimited(t, l)

		require.Equal(t, http.StatusTeapot, do(h, "GET", "shop-a.test", "/me", cookie("shop-a:visitor")).Code)
		<-seen
		assert.Equal(t, []string{"shop-a/visitor/user-shop-a:visitor"}, l.calls)
	})

	t.Run("platform hosts by address", func(t *testing.T) {
		l := &recordingLimiter{}
		h, _, seen := setupLimited(t, l)

		require.Equal(t, http.StatusTeapot, do(h, "POST", "admin.test", "/login", cookie("shop-a:visitor")).Code)
		<-seen
		assert.Equal(t, []string{"_platform/visitor/addr:192.0.2.1"}, l.calls)
	})
}

func TestIdentityDown(t *testing.T) {
	h, id, _ := setup(t)
	id.failing = true

	w := do(h, "GET", "shop-a.test", "/me", bearer("shop-a:customer"))

	assert.Equal(t, http.StatusBadGateway, w.Code)
	assert.Equal(t, ycerrors.CodeUpstreamFailure, problemCode(t, w))
}

func TestHealthBypassesTenantResolution(t *testing.T) {
	h, _, _ := setup(t)

	w := do(h, "GET", "unknown.test", "/healthz")

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpstreamDown(t *testing.T) {
	telemetry.InitForTest(t, "yc-gateway")
	routes, err := domain.NewRoutes([]domain.Route{{Prefix: "/me", Upstream: &url.URL{Scheme: "http", Host: "127.0.0.1:1"}}})
	require.NoError(t, err)
	gw := transport.New(hosts{"shop-a.test": "shop-a"}, &fakeIdentity{}, routes, nil, transport.CookieConfig{Name: "yc_visitor"})
	h := transport.NewRouter(gw, httpkit.BuildInfo{Service: "yc-gateway"}, nil)

	w := do(h, "GET", "shop-a.test", "/me")

	assert.Equal(t, http.StatusBadGateway, w.Code)
	assert.Equal(t, ycerrors.CodeUpstreamFailure, problemCode(t, w))
}
