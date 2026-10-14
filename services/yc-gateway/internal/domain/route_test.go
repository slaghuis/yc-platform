package domain_test

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	"github.com/slaghuis/yc-platform/services/yc-gateway/internal/domain"
)

func u(s string) *url.URL {
	v, _ := url.Parse(s)
	return v
}

func TestRoutesMatch(t *testing.T) {
	routes, err := domain.NewRoutes([]domain.Route{
		{Prefix: "/me", Methods: []string{"get"}, Upstream: u("http://identity")},
		{Prefix: "/products", Upstream: u("http://product")},
		{Prefix: "/products/admin", Upstream: u("http://admin")},
	})
	require.NoError(t, err)

	for _, tc := range []struct {
		method, path, want string
	}{
		{"GET", "/me", "http://identity"},
		{"GET", "/me/settings", "http://identity"},
		{"POST", "/me", ""},
		{"GET", "/meow", ""},
		{"GET", "/products/admin/1", "http://admin"},
		{"DELETE", "/products/1", "http://product"},
		{"GET", "/internal/validate-token", ""},
		{"GET", "/", ""},
	} {
		r, ok := routes.Match(tc.method, tc.path)
		if tc.want == "" {
			assert.False(t, ok, "%s %s", tc.method, tc.path)
			continue
		}
		require.True(t, ok, "%s %s", tc.method, tc.path)
		assert.Equal(t, tc.want, r.Upstream.String(), "%s %s", tc.method, tc.path)
	}
}

func TestNewRoutesRejects(t *testing.T) {
	for name, r := range map[string]domain.Route{
		"internal":     {Prefix: "/internal", Upstream: u("http://x")},
		"internal sub": {Prefix: "/internal/x", Upstream: u("http://x")},
		"relative":     {Prefix: "me", Upstream: u("http://x")},
		"no upstream":  {Prefix: "/me"},
		"bad role":     {Prefix: "/me", Upstream: u("http://x"), Roles: []authctx.ActorType{"root"}},
	} {
		_, err := domain.NewRoutes([]domain.Route{r})
		assert.Error(t, err, name)
	}
}

func TestRouteAdmits(t *testing.T) {
	open := domain.Route{}
	assert.True(t, open.Admits(authctx.ActorVisitor))
	admin := domain.Route{Roles: []authctx.ActorType{authctx.ActorShopAdmin}}
	assert.False(t, admin.Admits(authctx.ActorVisitor))
	assert.True(t, admin.Admits(authctx.ActorShopAdmin))
}
