package httpkit

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// BuildInfo is served at /version.
type BuildInfo struct {
	Service string `json:"service"`
	Version string `json:"version"`
	BuiltAt string `json:"built_at,omitempty"`
}

// RouterOptions configures NewRouter.
type RouterOptions struct {
	Build BuildInfo

	// Checks run on /readyz. Each must be fast; see Check.
	Checks []Check

	// Middleware runs on every service route, after correlation and
	// tracing and before the access log. Backend services pass
	// authctx.Middleware; yc-gateway passes tenant resolution and auth.
	Middleware []func(http.Handler) http.Handler
}

// Router is a chi.Router whose routes get the service middleware and which
// serves through the root mux, so the standard chain always runs.
type Router struct {
	chi.Router
	root *chi.Mux
}

// ServeHTTP serves through the root mux. Serving the inline router directly
// would skip the root middleware.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.root.ServeHTTP(w, req)
}

// NewRouter returns a router with the standard middleware chain and the
// /healthz, /readyz and /version endpoints. Services register their routes
// on the returned router.
//
// Chain, outermost first: correlation ID, tracing, opts.Middleware, access
// log, panic recovery.
func NewRouter(opts RouterOptions) *Router {
	r := chi.NewRouter()
	r.Use(
		CorrelationID,
		otelhttp.NewMiddleware(opts.Build.Service,
			otelhttp.WithFilter(func(r *http.Request) bool { return !isHealthPath(r.URL.Path) }),
		),
	)
	r.NotFound(chain(func(w http.ResponseWriter, r *http.Request) {
		writeNotFound(w, r)
	}, AccessLog))

	health := r.With(AccessLog, Recoverer)
	health.Get(pathHealthz, healthz)
	health.Get(pathReadyz, readyz(opts.Checks))
	health.Get(pathVersion, version(opts.Build))

	mw := make([]func(http.Handler) http.Handler, 0, len(opts.Middleware)+2)
	mw = append(mw, opts.Middleware...)
	mw = append(mw, AccessLog, Recoverer)
	return &Router{Router: r.With(mw...), root: r}
}

func chain(h http.HandlerFunc, mw ...func(http.Handler) http.Handler) http.HandlerFunc {
	var out http.Handler = h
	for i := len(mw) - 1; i >= 0; i-- {
		out = mw[i](out)
	}
	return out.ServeHTTP
}
