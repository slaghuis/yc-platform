package httpkit

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
)

const (
	pathHealthz = "/healthz"
	pathReadyz  = "/readyz"
	pathVersion = "/version"

	checkTimeout = 2 * time.Second
)

// Check is one readiness dependency (database, NATS, migrations). Fn must
// return quickly; it runs with a 2 s timeout on every /readyz call.
type Check struct {
	Name string
	Fn   func(ctx context.Context) error
}

// Readiness is the /readyz body.
type Readiness struct {
	Status string            `json:"status"` // ready | not_ready
	Checks map[string]string `json:"checks"` // name → ok | failing
}

func isHealthPath(p string) bool {
	return p == pathHealthz || p == pathReadyz || p == pathVersion
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

func readyz(checks []Check) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), checkTimeout)
		defer cancel()

		res := Readiness{Status: "ready", Checks: make(map[string]string, len(checks))}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, c := range checks {
			wg.Go(func() {
				err := c.Fn(ctx)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					res.Status = "not_ready"
					res.Checks[c.Name] = "failing"
					// Details stay in logs: /readyz is unauthenticated.
					telemetry.LoggerFromContext(ctx).Warn("readiness check failing",
						slog.String("check", c.Name), slog.Any("err", err))
					return
				}
				res.Checks[c.Name] = "ok"
			})
		}
		wg.Wait()

		status := http.StatusOK
		if res.Status != "ready" {
			status = http.StatusServiceUnavailable
		}
		WriteJSON(w, status, res)
	}
}

func version(b BuildInfo) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusOK, b)
	}
}

func writeNotFound(w http.ResponseWriter, r *http.Request) {
	ycerrors.WriteError(w, r, ycerrors.New(ycerrors.CodeNotFound, "Route not found").
		WithInternal("%s %s", r.Method, r.URL.Path))
}
