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

// InternalPrefix is never routed on the public listener: /internal/
// endpoints are for service-to-service calls through the internal
// listener (ADR-021 rule 3).
const InternalPrefix = "/internal/"

// gatewayInternal are the internal listener's own endpoints; no service
// may register them.
var gatewayInternal = []string{
	"/internal/register", "/internal/deregister", "/internal/routes",
	"/internal/service-token", "/internal/validate-token",
}

// IsInternal reports whether path is under InternalPrefix.
func IsInternal(path string) bool {
	return strings.HasPrefix(path, InternalPrefix) || path == strings.TrimSuffix(InternalPrefix, "/")
}

// Route forwards requests under Prefix to Upstream.
type Route struct {
	Prefix   string
	Methods  []string // upper case; empty means any method
	Upstream *url.URL
	// Roles admitted; empty admits any actor, visitors included.
	Roles []authctx.ActorType
	// Anonymous routes admit requests without a token on platform hosts,
	// where no visitor is issued (ADR-020 rule 5).
	Anonymous bool
	// Service is the registered service that owns the route; "" for
	// static routes.
	Service string
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

// Routes is a routing table. Internal routes are kept apart: only
// MatchInternal finds them.
type Routes struct {
	byLength []Route // longest prefix first
	internal []Route // longest prefix first
}

// NewRoutes validates routes and returns a table.
func NewRoutes(routes []Route) (*Routes, error) {
	out := make([]Route, 0, len(routes))
	var internal []Route
	for _, r := range routes {
		if !strings.HasPrefix(r.Prefix, "/") {
			return nil, fmt.Errorf("route %q: prefix must start with /", r.Prefix)
		}
		isInternal := IsInternal(r.Prefix)
		if isInternal {
			if err := validInternal(r); err != nil {
				return nil, err
			}
		}
		if r.Upstream == nil || r.Upstream.Host == "" {
			return nil, fmt.Errorf("route %q: upstream URL required", r.Prefix)
		}
		for _, role := range r.Roles {
			if !role.Valid() {
				return nil, fmt.Errorf("route %q: unknown role %q", r.Prefix, role)
			}
		}
		if r.Anonymous && len(r.Roles) > 0 {
			return nil, fmt.Errorf("route %q: an anonymous route cannot require roles", r.Prefix)
		}
		methods := make([]string, len(r.Methods))
		for i, m := range r.Methods {
			methods[i] = strings.ToUpper(m)
		}
		r.Methods = methods
		if isInternal {
			internal = append(internal, r)
		} else {
			out = append(out, r)
		}
	}
	longestFirst := func(rs []Route) {
		sort.SliceStable(rs, func(i, j int) bool { return len(rs[i].Prefix) > len(rs[j].Prefix) })
	}
	longestFirst(out)
	longestFirst(internal)
	return &Routes{byLength: out, internal: internal}, nil
}

// validInternal checks an internal route: only a registered service may
// own one, it admits service tokens only, and it may not shadow the
// gateway's own internal endpoints.
func validInternal(r Route) error {
	if r.Service == "" {
		return fmt.Errorf("route %q: %s paths are never routed statically", r.Prefix, InternalPrefix)
	}
	if len(r.Prefix) <= len(InternalPrefix) {
		return fmt.Errorf("route %q: an internal route needs a path below %s", r.Prefix, InternalPrefix)
	}
	if len(r.Roles) > 0 || r.Anonymous {
		return fmt.Errorf("route %q: internal routes admit service tokens only", r.Prefix)
	}
	for _, own := range gatewayInternal {
		if r.matches(own) || strings.HasPrefix(r.Prefix, own) {
			return fmt.Errorf("route %q: %s belongs to the gateway", r.Prefix, own)
		}
	}
	return nil
}

// All returns the public routes then the internal ones, each longest
// prefix first.
func (t *Routes) All() []Route {
	return slices.Concat(t.byLength, t.internal)
}

// Match returns the public route with the longest prefix of path that
// allows method. Internal paths never match.
func (t *Routes) Match(method, path string) (Route, bool) {
	if IsInternal(path) {
		return Route{}, false
	}
	return match(t.byLength, method, path)
}

// MatchInternal returns the internal route for a service-to-service call.
func (t *Routes) MatchInternal(method, path string) (Route, bool) {
	return match(t.internal, method, path)
}

func match(routes []Route, method, path string) (Route, bool) {
	for _, r := range routes {
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
