// Package domain holds yc-gateway's routing and identity types. It does no
// I/O.
package domain

import (
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
)

// InternalPrefix is never routed: /internal/ endpoints are for
// service-to-service calls inside the platform network.
const InternalPrefix = "/internal/"

// Route forwards requests under Prefix to Upstream.
type Route struct {
	Prefix   string
	Methods  []string // upper case; empty means any method
	Upstream *url.URL
	// Roles admitted; empty admits any actor, visitors included.
	Roles []authctx.ActorType
}

// Admits reports whether an actor of type t may use the route.
func (r Route) Admits(t authctx.ActorType) bool {
	return len(r.Roles) == 0 || slices.Contains(r.Roles, t)
}

// matches reports whether path is Prefix or below it: "/me" matches "/me"
// and "/me/x" but not "/meow".
func (r Route) matches(path string) bool {
	if !strings.HasPrefix(path, r.Prefix) {
		return false
	}
	return len(path) == len(r.Prefix) || strings.HasSuffix(r.Prefix, "/") || path[len(r.Prefix)] == '/'
}

// Routes is a routing table.
type Routes struct {
	byLength []Route // longest prefix first
}

// NewRoutes validates routes and returns a table.
func NewRoutes(routes []Route) (*Routes, error) {
	out := make([]Route, 0, len(routes))
	for _, r := range routes {
		if !strings.HasPrefix(r.Prefix, "/") {
			return nil, fmt.Errorf("route %q: prefix must start with /", r.Prefix)
		}
		if strings.HasPrefix(r.Prefix+"/", InternalPrefix) {
			return nil, fmt.Errorf("route %q: %s paths are never routed", r.Prefix, InternalPrefix)
		}
		if r.Upstream == nil || r.Upstream.Host == "" {
			return nil, fmt.Errorf("route %q: upstream URL required", r.Prefix)
		}
		for _, role := range r.Roles {
			if !role.Valid() {
				return nil, fmt.Errorf("route %q: unknown role %q", r.Prefix, role)
			}
		}
		for i, m := range r.Methods {
			r.Methods[i] = strings.ToUpper(m)
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].Prefix) > len(out[j].Prefix) })
	return &Routes{byLength: out}, nil
}

// Match returns the route with the longest prefix of path that allows
// method.
func (t *Routes) Match(method, path string) (Route, bool) {
	if strings.HasPrefix(path, InternalPrefix) || path == strings.TrimSuffix(InternalPrefix, "/") {
		return Route{}, false
	}
	for _, r := range t.byLength {
		if r.matches(path) && (len(r.Methods) == 0 || slices.Contains(r.Methods, method)) {
			return r, true
		}
	}
	return Route{}, false
}

// Identity is a validated caller.
type Identity struct {
	Token     string
	TokenID   string
	TenantID  string
	ActorID   string
	ActorType authctx.ActorType
	ExpiresAt time.Time
}
