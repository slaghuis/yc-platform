// Package http is yc-gateway's HTTP transport: tenant resolution,
// authentication and the reverse proxy.
package http

import (
	"context"
	"errors"
	"net/http"
	"net/http/httputil"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	"github.com/slaghuis/yc-platform/services/yc-gateway/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-gateway/internal/integration"
)

// TenantResolver maps a Host header to a tenant.
type TenantResolver interface {
	Resolve(host string) (string, bool)
}

// Authenticator validates tokens and issues visitor tokens.
type Authenticator interface {
	Validate(ctx context.Context, token string) (domain.Identity, error)
	CreateVisitor(ctx context.Context, tenantID string) (domain.Identity, error)
}

// CookieConfig is the visitor cookie.
type CookieConfig struct {
	Name   string `koanf:"name"`
	Secure bool   `koanf:"secure"`
}

// Gateway authenticates requests and proxies them to backends.
type Gateway struct {
	hosts  TenantResolver
	auth   Authenticator
	routes *domain.Routes
	cookie CookieConfig
	proxy  *httputil.ReverseProxy
}

// New returns a Gateway.
func New(hosts TenantResolver, auth Authenticator, routes *domain.Routes, cookie CookieConfig) *Gateway {
	g := &Gateway{hosts: hosts, auth: auth, routes: routes, cookie: cookie}
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
		Middleware: []func(http.Handler) http.Handler{g.Authenticate},
	})
	r.Handle("/*", g)
	return r
}

type routeKey struct{}

// Authenticate resolves the tenant and route, establishes the caller and
// puts both on the context. Without a token it issues a visitor token and
// sets the visitor cookie.
func (g *Gateway) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only the gateway may assert identity downstream.
		authctx.StripHeaders(r.Header)

		tenantID, ok := g.hosts.Resolve(r.Host)
		if !ok {
			ycerrors.WriteError(w, r, ycerrors.New(ycerrors.CodeNotFound, "Unknown shop").
				WithInternal("no tenant for host %q", r.Host))
			return
		}
		route, ok := g.routes.Match(r.Method, r.URL.Path)
		if !ok {
			ycerrors.WriteError(w, r, ycerrors.New(ycerrors.CodeNotFound, "Route not found").
				WithInternal("%s %s", r.Method, r.URL.Path))
			return
		}

		id, err := g.identify(w, r, tenantID)
		if err != nil {
			ycerrors.WriteError(w, r, err)
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

// identify returns the caller for tenantID. A bearer token must be valid
// for the tenant. The visitor cookie is the gateway's own state, so a
// stale or foreign one is replaced with a fresh visitor rather than
// rejected.
func (g *Gateway) identify(w http.ResponseWriter, r *http.Request, tenantID string) (domain.Identity, error) {
	ctx := r.Context()
	if bearer := authctx.BearerToken(r.Header); bearer != "" {
		id, err := g.auth.Validate(ctx, bearer)
		if errors.Is(err, integration.ErrInvalidToken) {
			return domain.Identity{}, ycerrors.Unauthorized("Invalid or expired token")
		}
		if err != nil {
			return domain.Identity{}, err
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
