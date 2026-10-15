package http_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	"github.com/slaghuis/yc-platform/services/yc-gateway/internal/domain"
	transport "github.com/slaghuis/yc-platform/services/yc-gateway/internal/transport/http"
)

type registrar struct{ routes *domain.Routes }

func (registrar) Register(context.Context, httpkit.Registration) error     { return nil }
func (registrar) Deregister(context.Context, string, string, string) error { return nil }
func (r registrar) Routes() []domain.Route                                 { return r.routes.All() }

type internalSeen struct {
	upstreamSeen
	Caller, Body string
}

func setupInternal(t *testing.T) (http.Handler, chan internalSeen) {
	t.Helper()
	telemetry.InitForTest(t, "yc-gateway")

	seen := make(chan internalSeen, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen <- internalSeen{
			upstreamSeen: upstreamSeen{
				Path:        r.URL.Path,
				Auth:        r.Header.Get("Authorization"),
				Tenant:      r.Header.Get(authctx.HeaderTenantID),
				Actor:       r.Header.Get(authctx.HeaderActorID),
				ActorType:   r.Header.Get(authctx.HeaderActorType),
				Correlation: r.Header.Get(authctx.HeaderCorrelationID),
			},
			Caller: r.Header.Get(authctx.HeaderCallerService),
			Body:   string(body),
		}
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(backend.Close)
	up, err := url.Parse(backend.URL)
	require.NoError(t, err)

	routes, err := domain.NewRoutes([]domain.Route{
		{Prefix: "/prices", Upstream: up, Service: "yc-pricing"},
		{Prefix: "/internal/evaluate", Methods: []string{"POST"}, Upstream: up, Service: "yc-pricing"},
	})
	require.NoError(t, err)
	in := transport.NewInternal(&fakeIdentity{}, routes, up)
	return transport.NewInternalRouter(registrar{routes}, in, httpkit.BuildInfo{Service: "yc-gateway"}), seen
}

func doInternal(h http.Handler, method, path string, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(`{"x":1}`))
	for _, m := range mutate {
		m(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func headers(kv ...string) func(*http.Request) {
	return func(r *http.Request) {
		for i := 0; i < len(kv); i += 2 {
			r.Header.Set(kv[i], kv[i+1])
		}
	}
}

func TestInternalCallForwardsCaller(t *testing.T) {
	h, seen := setupInternal(t)
	const corrID = "0192f0c1-6a2b-7c3d-8e4f-5a6b7c8d9e0f"

	w := doInternal(h, "POST", "/internal/evaluate", bearer("_platform:service"), headers(
		authctx.HeaderTenantID, "shop-a",
		authctx.HeaderActorID, "u1",
		authctx.HeaderActorType, "customer",
		authctx.HeaderCorrelationID, corrID,
		authctx.HeaderCallerService, "yc-spoofed",
	))

	require.Equal(t, http.StatusTeapot, w.Code)
	got := <-seen
	assert.Equal(t, internalSeen{
		upstreamSeen: upstreamSeen{
			Path: "/internal/evaluate", Tenant: "shop-a", Actor: "u1", ActorType: "customer", Correlation: corrID,
		},
		Caller: "user-_platform:service",
		Body:   `{"x":1}`,
	}, got)
}

func TestInternalCallWithoutActorActsAsService(t *testing.T) {
	h, seen := setupInternal(t)

	w := doInternal(h, "POST", "/internal/evaluate", bearer("_platform:service"),
		headers(authctx.HeaderTenantID, "shop-a"))

	require.Equal(t, http.StatusTeapot, w.Code)
	got := <-seen
	assert.Equal(t, "service", got.ActorType)
	assert.Equal(t, "user-_platform:service", got.Actor)
}

func TestServiceTokenProxiedToIdentity(t *testing.T) {
	h, seen := setupInternal(t)

	w := doInternal(h, "POST", "/internal/service-token", headers(authctx.HeaderTenantID, "shop-a"))

	require.Equal(t, http.StatusTeapot, w.Code)
	got := <-seen
	assert.Equal(t, "/internal/service-token", got.Path)
	assert.Equal(t, "yc-gateway", got.Caller)
	assert.Empty(t, got.Tenant, "identity headers are stripped")
	assert.Equal(t, `{"x":1}`, got.Body)
}

func TestInternalRejections(t *testing.T) {
	tenant := headers(authctx.HeaderTenantID, "shop-a")
	for _, tc := range []struct {
		name, method, path string
		mutate             []func(*http.Request)
		status             int
		code               ycerrors.Code
	}{
		{"no token", "POST", "/internal/evaluate", []func(*http.Request){tenant}, 401, ycerrors.CodeUnauthorized},
		{"invalid token", "POST", "/internal/evaluate", []func(*http.Request){tenant, bearer("garbage")}, 401, ycerrors.CodeUnauthorized},
		{"user token", "POST", "/internal/evaluate", []func(*http.Request){tenant, bearer("shop-a:customer")}, 401, ycerrors.CodeUnauthorized},
		{"no route", "POST", "/internal/nothing", []func(*http.Request){tenant, bearer("_platform:service")}, 503, ycerrors.CodeUnavailable},
		{"wrong method", "GET", "/internal/evaluate", []func(*http.Request){tenant, bearer("_platform:service")}, 503, ycerrors.CodeUnavailable},
		{"public route", "GET", "/internal/prices", []func(*http.Request){tenant, bearer("_platform:service")}, 503, ycerrors.CodeUnavailable},
		{"no tenant", "POST", "/internal/evaluate", []func(*http.Request){bearer("_platform:service")}, 400, ycerrors.CodeValidationFailed},
		{"actor type without ID", "POST", "/internal/evaluate", []func(*http.Request){
			tenant, bearer("_platform:service"), headers(authctx.HeaderActorType, "customer"),
		}, 400, ycerrors.CodeValidationFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, seen := setupInternal(t)

			w := doInternal(h, tc.method, tc.path, tc.mutate...)

			assert.Equal(t, tc.status, w.Code)
			assert.Equal(t, tc.code, problemCode(t, w))
			assert.Empty(t, seen)
		})
	}
}
