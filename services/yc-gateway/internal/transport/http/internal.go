package http

import (
	"context"
	"net/http"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	"github.com/slaghuis/yc-platform/services/yc-gateway/internal/domain"
)

// Registrar holds service registrations.
type Registrar interface {
	Register(ctx context.Context, req httpkit.Registration) error
	Deregister(ctx context.Context, serviceName, instanceID, reason string) error
	Routes() []domain.Route
}

// RouteView is one entry of GET /internal/routes.
type RouteView struct {
	Prefix    string   `json:"prefix"`
	Methods   []string `json:"methods,omitempty"`
	Upstream  string   `json:"upstream"`
	Roles     []string `json:"roles,omitempty"`
	Anonymous bool     `json:"anonymous,omitempty"`
	Service   string   `json:"service,omitempty"` // empty for static routes
}

// NewInternalRouter serves the registration endpoints on the internal
// listener (ADR-020 rule 6). It has no authentication: the listener must
// not be reachable from outside the platform network.
func NewInternalRouter(reg Registrar, build httpkit.BuildInfo) http.Handler {
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
				Roles: roles, Anonymous: rt.Anonymous, Service: rt.Service,
			}
		}
		httpkit.WriteJSON(w, http.StatusOK, map[string]any{"routes": out})
	})
	return r
}
