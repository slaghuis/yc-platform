// Package http is yc-gateway's HTTP transport: tenant resolution,
// authentication and the reverse proxy.
package http

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	"github.com/slaghuis/yc-platform/services/yc-gateway/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-gateway/internal/integration"
	"github.com/slaghuis/yc-platform/services/yc-gateway/internal/service"
)

// TenantResolver maps a Host header to a tenant and its lifecycle state.
type TenantResolver interface {
	Lookup(host string) (tenantID string, state service.HostState)
}

// Authenticator validates tokens and issues visitor tokens.
type Authenticator interface {
	Validate(ctx context.Context, token string) (domain.Identity, error)
	CreateVisitor(ctx context.Context, tenantID string) (domain.Identity, error)
}

// RouteMatcher finds the route for a request.
type RouteMatcher interface {
	Match(method, path string) (domain.Route, bool)
}

// Limiter enforces rate limits. actorKey is the actor ID, or the client
// address for callers without credentials.
type Limiter interface {
	Allow(tenantID string, actorType authctx.ActorType, actorKey string) (bool, time.Duration)
}

type unlimited struct{}

func (unlimited) Allow(string, authctx.ActorType, string) (bool, time.Duration) { return true, 0 }

// CookieConfig is the visitor cookie.
type CookieConfig struct {
	Name   string `koanf:"name"`
	Secure bool   `koanf:"secure"`
}

// Gateway authenticates requests and proxies them to backends.
type Gateway struct {
	hosts   TenantResolver
	auth    Authenticator
	routes  RouteMatcher
	limiter Limiter
	cookie  CookieConfig
	trusted []netip.Prefix
	proxy   *httputil.ReverseProxy
}

// New returns a Gateway. A nil limiter admits everything.
func New(hosts TenantResolver, auth Authenticator, routes RouteMatcher, limiter Limiter, cookie CookieConfig) *Gateway {
	if limiter == nil {
		limiter = unlimited{}
	}
	g := &Gateway{hosts: hosts, auth: auth, routes: routes, limiter: limiter, cookie: cookie}
	g.proxy = &httputil.ReverseProxy{
		Rewrite:      g.rewrite,
		Transport:    otelhttp.NewTransport(http.DefaultTransport),
		ErrorHandler: proxyError,
	}
	return g
}

// NewRouter mounts the gateway on the standard httpkit chain. Health
// endpoints are the gateway's own; every other path is proxied.
func NewRouter(g *Gateway, build httpkit.BuildInfo, checks []httpkit.Check) http.Handler {
	r := httpkit.NewRouter(httpkit.RouterOptions{
		Build:      build,
		Checks:     checks,
		Middleware: []func(http.Handler) http.Handler{securityHeaders, g.Authenticate},
	})
	r.Handle("/*", g)
	return r
}

// securityHeaders sets the browser hardening headers on every public
// response. The platform serves only JSON and redirects, so nothing may be
// framed, sniffed or loaded. Browsers ignore HSTS over plain HTTP, so it is
// safe in local development.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Strict-Transport-Security", "max-age=31536000")
		next.ServeHTTP(w, r)
	})
}

type routeKey struct{}

// Authenticate resolves the tenant and route, establishes the caller and
// puts both on the context. Without a token it issues a visitor token and
// sets the visitor cookie.
func (g *Gateway) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only the gateway may assert identity downstream.
		authctx.StripHeaders(r.Header)

		tenantID, hostState := g.hosts.Lookup(r.Host)
		switch hostState {
		case service.HostUnknown:
			ycerrors.WriteError(w, r, ycerrors.New(ycerrors.CodeNotFound, "Unknown shop").
				WithInternal("no tenant for host %q", r.Host))
			return
		case service.HostDeleted:
			ycerrors.WriteError(w, r, ycerrors.Gone("This shop is no longer available").
				WithInternal("deleted tenant %s host %q", tenantID, r.Host))
			return
		case service.HostSuspended:
			ycerrors.WriteError(w, r, ycerrors.New(ycerrors.CodeTenantSuspended, "This shop is temporarily unavailable").
				WithInternal("suspended tenant %s host %q", tenantID, r.Host))
			return
		case service.HostActive:
			// continue
		}
		route, ok := g.routes.Match(r.Method, r.URL.Path)
		if !ok {
			ycerrors.WriteError(w, r, ycerrors.New(ycerrors.CodeNotFound, "Route not found").
				WithInternal("%s %s", r.Method, r.URL.Path))
			return
		}

		// Third-party callbacks carry no token and must not mint visitors;
		// the upstream verifies them.
		if route.Callback {
			if !g.allow(w, r, tenantID, authctx.ActorVisitor, "addr:"+g.clientAddr(r)) {
				return
			}
			ctx := authctx.WithAuth(r.Context(), authctx.AuthContext{
				TenantID:      tenantID,
				CorrelationID: authctx.CorrelationID(r.Context()),
			})
			ctx = context.WithValue(ctx, routeKey{}, route)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		// Callers without credentials are limited by address before a
		// visitor is issued for them.
		bearer := authctx.BearerToken(r.Header)
		anonymous := bearer == "" && !hasCookie(r, g.cookie.Name)
		if anonymous && !g.allow(w, r, tenantID, authctx.ActorVisitor, "addr:"+g.clientAddr(r)) {
			return
		}

		// Platform hosts never issue visitors (ADR-020 rule 5): without a
		// token only anonymous routes are reachable, with no actor.
		if tenantID == eventbus.PlatformTenantID && bearer == "" {
			if !anonymous && !g.allow(w, r, tenantID, authctx.ActorVisitor, "addr:"+g.clientAddr(r)) {
				return
			}
			if !route.Anonymous {
				ycerrors.WriteError(w, r, ycerrors.Unauthorized("Sign in required"))
				return
			}
			ctx := authctx.WithAuth(r.Context(), authctx.AuthContext{
				TenantID:      tenantID,
				CorrelationID: authctx.CorrelationID(r.Context()),
			})
			ctx = context.WithValue(ctx, routeKey{}, route)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		id, err := g.identify(w, r, tenantID)
		if err != nil {
			ycerrors.WriteError(w, r, err)
			return
		}
		if !anonymous && !g.allow(w, r, tenantID, id.ActorType, id.ActorID) {
			return
		}
		if !route.Admits(id.ActorType) {
			if id.ActorType == authctx.ActorVisitor {
				ycerrors.WriteError(w, r, ycerrors.Unauthorized("Sign in required"))
			} else {
				ycerrors.WriteError(w, r, ycerrors.Forbidden("Role not permitted").
					WithInternal("actor_type %s, route %s admits %v", id.ActorType, route.Prefix, route.Roles))
			}
			return
		}

		ctx := authctx.WithAuth(r.Context(), authctx.AuthContext{
			TenantID:       id.TenantID,
			ActorID:        id.ActorID,
			ActorType:      id.ActorType,
			CorrelationID:  authctx.CorrelationID(r.Context()),
			AccessTokenJTI: id.TokenID,
			Token:          id.Token,
		})
		ctx = context.WithValue(ctx, routeKey{}, route)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// allow applies the rate limits, writing a 429 with Retry-After when the
// request is refused.
func (g *Gateway) allow(w http.ResponseWriter, r *http.Request, tenantID string, actorType authctx.ActorType, actorKey string) bool {
	if err := g.limit(w, tenantID, actorType, actorKey); err != nil {
		ycerrors.WriteError(w, r, err)
		return false
	}
	return true
}

// limit applies the rate limits, setting Retry-After and returning the 429
// when the request is refused.
func (g *Gateway) limit(w http.ResponseWriter, tenantID string, actorType authctx.ActorType, actorKey string) error {
	ok, wait := g.limiter.Allow(tenantID, actorType, actorKey)
	if ok {
		return nil
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
	return ycerrors.RateLimited("Too many requests; try again later").
		WithInternal("tenant %s, %s %s, wait %s", tenantID, actorType, actorKey, wait)
}

func hasCookie(r *http.Request, name string) bool {
	c, err := r.Cookie(name)
	return err == nil && c.Value != ""
}

// TrustProxies sets the load balancers in front of the gateway. A request
// from one of them is attributed to the rightmost X-Forwarded-For address
// outside prefixes: entries to its left are the client's to forge. Without
// trusted proxies X-Forwarded-For is ignored, so behind a load balancer
// every caller without credentials would share its address.
func (g *Gateway) TrustProxies(prefixes []netip.Prefix) { g.trusted = prefixes }

func (g *Gateway) isTrusted(addr netip.Addr) bool {
	for _, p := range g.trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// clientAddr is the caller's IP: the connection's remote address, or the
// client a trusted proxy forwarded for.
func (g *Gateway) clientAddr(r *http.Request) string {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	addr := ap.Addr().Unmap()
	if !g.isTrusted(addr) {
		return addr.String()
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		addr = hop.Unmap()
		if !g.isTrusted(addr) {
			break
		}
	}
	return addr.String()
}

// identify returns the caller for tenantID. A bearer token must be valid
// for the tenant. The visitor cookie is the gateway's own state, so a
// stale or foreign one is replaced with a fresh visitor rather than
// rejected. Invalid credentials count against the caller's address, like
// no credentials, so they cannot be used to dodge the limits.
func (g *Gateway) identify(w http.ResponseWriter, r *http.Request, tenantID string) (domain.Identity, error) {
	ctx := r.Context()
	byAddr := func() error { return g.limit(w, tenantID, authctx.ActorVisitor, "addr:"+g.clientAddr(r)) }
	if bearer := authctx.BearerToken(r.Header); bearer != "" {
		id, err := g.auth.Validate(ctx, bearer)
		if errors.Is(err, integration.ErrInvalidToken) {
			if err := byAddr(); err != nil {
				return domain.Identity{}, err
			}
			return domain.Identity{}, ycerrors.Unauthorized("Invalid or expired token")
		}
		if err != nil {
			return domain.Identity{}, err
		}
		if id.ActorType == authctx.ActorService {
			return domain.Identity{}, ycerrors.Unauthorized("Invalid or expired token").
				WithInternal("service token %s on the public listener", id.ActorID)
		}
		if id.TenantID != tenantID {
			return domain.Identity{}, ycerrors.Forbidden("Token is not valid for this shop").
				WithInternal("token tenant %s, host tenant %s", id.TenantID, tenantID)
		}
		return id, nil
	}

	if c, err := r.Cookie(g.cookie.Name); err == nil && c.Value != "" {
		id, err := g.auth.Validate(ctx, c.Value)
		switch {
		case err == nil && id.TenantID == tenantID:
			return id, nil
		case err != nil && !errors.Is(err, integration.ErrInvalidToken):
			return domain.Identity{}, err
		}
		if err := byAddr(); err != nil {
			return domain.Identity{}, err
		}
		telemetry.LoggerFromContext(ctx).Info("replacing stale visitor cookie")
	}

	id, err := g.auth.CreateVisitor(ctx, tenantID)
	if err != nil {
		return domain.Identity{}, err
	}
	//nolint:gosec // Secure is configurable for plain-HTTP local development; on in deployed environments.
	http.SetCookie(w, &http.Cookie{
		Name:     g.cookie.Name,
		Value:    id.Token,
		Path:     "/",
		Expires:  id.ExpiresAt,
		HttpOnly: true,
		Secure:   g.cookie.Secure,
		SameSite: http.SameSiteLaxMode,
	})
	return id, nil
}

// ServeHTTP proxies an authenticated request to its route's upstream.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.proxy.ServeHTTP(w, r)
}

func (g *Gateway) rewrite(pr *httputil.ProxyRequest) {
	route, _ := pr.In.Context().Value(routeKey{}).(domain.Route)
	a, _ := authctx.FromContext(pr.In.Context())
	pr.SetURL(route.Upstream)
	pr.SetXForwarded()
	pr.Out.Header.Del(authctx.HeaderAuthorization)
	authctx.SetHeaders(pr.Out.Header, a)
	removeCookie(pr.Out, g.cookie.Name)
}

// removeCookie drops the visitor cookie from the upstream request; the
// token travels in Authorization.
func removeCookie(r *http.Request, name string) {
	cookies := r.Cookies()
	r.Header.Del("Cookie")
	var kept []string
	for _, c := range cookies {
		if c.Name != name {
			kept = append(kept, c.String())
		}
	}
	if len(kept) > 0 {
		r.Header.Set("Cookie", strings.Join(kept, "; "))
	}
}

func proxyError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	ycerrors.WriteError(w, r, ycerrors.UpstreamFailure("proxy "+r.Method+" "+r.URL.Path).Wrap(err))
}
