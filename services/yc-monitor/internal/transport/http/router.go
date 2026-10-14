// Package http is yc-monitor's HTTP transport.
package http

import (
	"context"
	"net/http"
	"strconv"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	"github.com/slaghuis/yc-platform/services/yc-monitor/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-monitor/internal/service"
)

// Monitor is the service the handlers call.
type Monitor interface {
	ListEvents(ctx context.Context, f domain.Filter) ([]domain.ReceivedEvent, error)
	ListDeadLetters(ctx context.Context, f domain.Filter) ([]domain.DLQEntry, error)
}

// NewRouter returns the service's routes on the standard httpkit chain.
func NewRouter(svc Monitor, build httpkit.BuildInfo, checks []httpkit.Check) http.Handler {
	h := handlers{svc: svc}
	r := httpkit.NewRouter(httpkit.RouterOptions{
		Build:      build,
		Checks:     checks,
		Middleware: []func(http.Handler) http.Handler{authctx.Middleware},
	})
	sysadmin := r.With(authctx.Require(authctx.ActorSystemAdmin))
	sysadmin.Get("/sysadmin/events", h.events)
	sysadmin.Get("/sysadmin/dlq", h.dlq)
	return r
}

type handlers struct {
	svc Monitor
}

// EventsResponse is the body of GET /sysadmin/events.
type EventsResponse struct {
	Events []domain.ReceivedEvent `json:"events"`
}

func (h handlers) events(w http.ResponseWriter, r *http.Request) {
	f, err := filter(r)
	if err != nil {
		ycerrors.WriteError(w, r, err)
		return
	}
	events, err := h.svc.ListEvents(r.Context(), f)
	if err != nil {
		ycerrors.WriteError(w, r, err)
		return
	}
	httpkit.WriteJSON(w, http.StatusOK, EventsResponse{Events: nonNil(events)})
}

// DLQResponse is the body of GET /sysadmin/dlq.
type DLQResponse struct {
	Entries []domain.DLQEntry `json:"entries"`
}

func (h handlers) dlq(w http.ResponseWriter, r *http.Request) {
	f, err := filter(r)
	if err != nil {
		ycerrors.WriteError(w, r, err)
		return
	}
	entries, err := h.svc.ListDeadLetters(r.Context(), f)
	if err != nil {
		ycerrors.WriteError(w, r, err)
		return
	}
	httpkit.WriteJSON(w, http.StatusOK, DLQResponse{Entries: nonNil(entries)})
}

// filter reads ?tenant_id= and ?limit= (1–service.MaxLimit, default 100).
func filter(r *http.Request) (domain.Filter, error) {
	q := r.URL.Query()
	f := domain.Filter{TenantID: q.Get("tenant_id")}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > service.MaxLimit {
			return f, ycerrors.Validation().
				Field("limit", "out_of_range", "limit must be between 1 and "+strconv.Itoa(service.MaxLimit)).
				Build()
		}
		f.Limit = n
	}
	return f, nil
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
