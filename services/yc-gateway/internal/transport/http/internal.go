package http

import (
	"context"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	"github.com/slaghuis/yc-platform/services/yc-gateway/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-gateway/internal/integration"
)

// Registrar holds service registrations.
type Registrar interface {
	Register(ctx context.Context, req httpkit.Registration) error
	Deregister(ctx context.Context, serviceName, instanceID, reason string) error
	Routes() []domain.Route
}

// InternalMatcher finds the registered route for a service-to-service
// call.
type InternalMatcher interface {
	MatchInternal(method, path string) (domain.Route, bool)
}

// RouteView is one entry of GET /internal/routes.
type RouteView struct {
	Prefix    string   `json:"prefix"`
	Methods   []string `json:"methods,omitempty"`
	Upstream  string   `json:"upstream"`
	Roles     []string `json:"roles,omitempty"`
	Anonymous bool     `json:"anonymous,omitempty"`
	Callback  bool     `json:"callback,omitempty"`
	Service   string   `json:"service,omitempty"` // empty for static routes
}

// gatewayService is the caller service yc-gateway names on its own calls.
const gatewayService = "yc-gateway"

// Internal routes service-to-service calls on the internal listener
// (ADR-021 rule 3).
type Internal struct {
	auth     Authenticator
	routes   InternalMatcher
	identity *url.URL
	proxy    *httputil.ReverseProxy
}

// NewInternal returns the internal call router. identity is yc-identity's
// base URL, which issues service tokens.
func NewInternal(auth Authenticator, routes InternalMatcher, identity *url.URL) *Internal {
	in := &Internal{auth: auth, routes: routes, identity: identity}
	in.proxy = &httputil.ReverseProxy{
		Rewrite:      rewriteInternal,
		Transport:    otelhttp.NewTransport(http.DefaultTransport),
		ErrorHandler: proxyError,
	}
	return in
}

type internalKey struct{}

// internalTarget is where and as whom an internal call is proxied.
type internalTarget struct {
	upstream *url.URL
	auth     authctx.AuthContext
	caller   string
}

// NewInternalRouter serves the registration endpoints (ADR-020 rule 6),
// the service-token endpoint and routed internal calls (ADR-021 rule 3).
// The registration endpoints have no authentication: the listener must not
// be reachable from outside the platform network.
func NewInternalRouter(reg Registrar, in *Internal, build httpkit.BuildInfo) http.Handler {
	r := httpkit.NewRouter(httpkit.RouterOptions{Build: build})
	r.Post("/internal/register", func(w http.ResponseWriter, r *http.Request) {
		var req httpkit.Registration
		if err := httpkit.DecodeJSON(w, r, &req); err != nil {
			ycerrors.WriteError(w, r, err)
			return
		}
		if err := reg.Register(r.Context(), req); err != nil {
			ycerrors.WriteError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	r.Post("/internal/deregister", func(w http.ResponseWriter, r *http.Request) {
		var req httpkit.Deregistration
		if err := httpkit.DecodeJSON(w, r, &req); err != nil {
			ycerrors.WriteError(w, r, err)
			return
		}
		err := reg.Deregister(r.Context(), req.ServiceName, req.InstanceID,
			events.PlatformServiceDeregisteredV1ReasonShutdown)
		if err != nil {
			ycerrors.WriteError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	r.Get("/internal/routes", func(w http.ResponseWriter, _ *http.Request) {
		routes := reg.Routes()
		out := make([]RouteView, len(routes))
		for i, rt := range routes {
			roles := make([]string, len(rt.Roles))
			for j, role := range rt.Roles {
				roles[j] = string(role)
			}
			out[i] = RouteView{
				Prefix: rt.Prefix, Methods: rt.Methods, Upstream: rt.Upstream.String(),
				Roles: roles, Anonymous: rt.Anonymous, Callback: rt.Callback, Service: rt.Service,
			}
		}
		httpkit.WriteJSON(w, http.StatusOK, map[string]any{"routes": out})
	})
	r.Post("/internal/service-token", in.serviceToken)
	r.Handle("/internal/*", in)
	return r
}

// serviceToken proxies a token request to yc-identity unauthenticated: the
// client secret in the body is the credential.
func (in *Internal) serviceToken(w http.ResponseWriter, r *http.Request) {
	ctx := context.WithValue(r.Context(), internalKey{}, internalTarget{
		upstream: in.identity,
		auth:     authctx.AuthContext{CorrelationID: authctx.CorrelationID(r.Context())},
		caller:   gatewayService,
	})
	in.proxy.ServeHTTP(w, r.WithContext(ctx))
}

// ServeHTTP authenticates a service-to-service call and proxies it to the
// service that registered its path.
func (in *Internal) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	target, err := in.resolve(r)
	if err != nil {
		ycerrors.WriteError(w, r, err)
		return
	}
	in.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), internalKey{}, target)))
}

func (in *Internal) resolve(r *http.Request) (internalTarget, error) {
	bearer := authctx.BearerToken(r.Header)
	if bearer == "" {
		return internalTarget{}, ycerrors.Unauthorized("Service token required")
	}
	id, err := in.auth.Validate(r.Context(), bearer)
	if errors.Is(err, integration.ErrInvalidToken) {
		return internalTarget{}, ycerrors.Unauthorized("Invalid or expired token")
	}
	if err != nil {
		return internalTarget{}, err
	}
	if id.ActorType != authctx.ActorService {
		return internalTarget{}, ycerrors.Unauthorized("Service token required").
			WithInternal("actor_type %s on %s", id.ActorType, r.URL.Path)
	}

	route, ok := in.routes.MatchInternal(r.Method, r.URL.Path)
	if !ok {
		return internalTarget{}, ycerrors.New(ycerrors.CodeNotFound, "Route not found").
			WithInternal("%s %s", r.Method, r.URL.Path)
	}

	a := authctx.AuthContext{
		TenantID:      r.Header.Get(authctx.HeaderTenantID),
		ActorID:       r.Header.Get(authctx.HeaderActorID),
		ActorType:     authctx.ActorType(r.Header.Get(authctx.HeaderActorType)),
		CorrelationID: authctx.CorrelationID(r.Context()),
	}
	v := ycerrors.Validation()
	if err := eventbus.ValidateTenantID(a.TenantID); err != nil {
		v.Field(authctx.HeaderTenantID, "invalid", "A valid tenant header is required")
	}
	switch {
	case a.ActorType == "" && a.ActorID == "":
		a.ActorType, a.ActorID = authctx.ActorService, id.ActorID
	case !a.ActorType.Valid() || a.ActorID == "":
		v.Field(authctx.HeaderActorType, "invalid", "Actor type and ID go together")
	}
	if v.HasErrors() {
		return internalTarget{}, v.Build()
	}
	return internalTarget{upstream: route.Upstream, auth: a, caller: id.ActorID}, nil
}

func rewriteInternal(pr *httputil.ProxyRequest) {
	t, _ := pr.In.Context().Value(internalKey{}).(internalTarget)
	pr.SetURL(t.upstream)
	pr.SetXForwarded()
	pr.Out.Header.Del(authctx.HeaderAuthorization)
	authctx.StripHeaders(pr.Out.Header)
	authctx.SetHeaders(pr.Out.Header, t.auth)
	pr.Out.Header.Set(authctx.HeaderCallerService, t.caller)
}
