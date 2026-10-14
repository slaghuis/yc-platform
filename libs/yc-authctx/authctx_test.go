package authctx_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
)

func TestWithAuthMirrorsTelemetry(t *testing.T) {
	t.Parallel()
	ctx := authctx.WithAuth(context.Background(), authctx.AuthContext{
		TenantID: "shop-abc", ActorID: "u1", ActorType: authctx.ActorVisitor, CorrelationID: "c1",
	})

	a, ok := authctx.FromContext(ctx)
	require.True(t, ok)
	assert.True(t, a.Authenticated())

	attrs := telemetry.AuthAttrsFromContext(ctx)
	assert.Equal(t, "shop-abc", attrs.TenantID)
	assert.Equal(t, "visitor", attrs.ActorType)
	assert.Equal(t, "c1", attrs.CorrelationID)
}

func TestWithCorrelationIDKeepsAuth(t *testing.T) {
	t.Parallel()
	ctx := authctx.WithAuth(context.Background(), authctx.AuthContext{TenantID: "t", ActorID: "a", ActorType: authctx.ActorCustomer})
	ctx = authctx.WithCorrelationID(ctx, "c2")

	a, _ := authctx.FromContext(ctx)
	assert.Equal(t, "t", a.TenantID)
	assert.Equal(t, "c2", authctx.CorrelationID(ctx))
}

func TestSetHeadersRoundTrip(t *testing.T) {
	t.Parallel()
	in := authctx.AuthContext{
		TenantID: "shop-abc", ActorID: "u1", ActorType: authctx.ActorCustomer,
		CorrelationID: "c1", Token: "tok",
	}
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	authctx.SetHeaders(req.Header, in)

	var got authctx.AuthContext
	h := authctx.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, _ = authctx.FromContext(r.Context())
	}))
	h.ServeHTTP(httptest.NewRecorder(), req)

	assert.Equal(t, in, got)
}

func TestStripHeaders(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	authctx.SetHeaders(h, authctx.AuthContext{TenantID: "x", ActorID: "y", ActorType: authctx.ActorSystemAdmin, CorrelationID: "c"})
	authctx.StripHeaders(h)
	assert.Empty(t, h.Get(authctx.HeaderTenantID))
	assert.Empty(t, h.Get(authctx.HeaderActorType))
	assert.Equal(t, "c", h.Get(authctx.HeaderCorrelationID), "correlation ID is not an identity header")
}

func TestBearerToken(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"Bearer abc":  "abc",
		"bearer  abc": "abc",
		"Basic abc":   "",
		"Bearer":      "",
		"":            "",
	}
	for in, want := range tests {
		h := http.Header{}
		if in != "" {
			h.Set("Authorization", in)
		}
		assert.Equal(t, want, authctx.BearerToken(h), in)
	}
}

func TestRequire(t *testing.T) {
	telemetry.InitForTest(t, "authctx-test")
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := authctx.Middleware(authctx.Require(authctx.ActorSystemAdmin)(ok))

	tests := []struct {
		name      string
		actorType string
		want      int
	}{
		{"no actor", "", http.StatusUnauthorized},
		{"wrong role", "visitor", http.StatusForbidden},
		{"allowed", "system_admin", http.StatusNoContent},
		{"unknown role", "superuser", http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/sysadmin/x", http.NoBody)
			if tt.actorType != "" {
				req.Header.Set(authctx.HeaderActorID, "a1")
				req.Header.Set(authctx.HeaderActorType, tt.actorType)
				req.Header.Set(authctx.HeaderTenantID, "_platform")
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			assert.Equal(t, tt.want, rec.Code)
		})
	}
}
