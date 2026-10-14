package httpkit

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
)

// CorrelationID takes X-YC-Correlation-ID from the request when it is a
// UUID, or generates one, puts it on the context and echoes it on the
// response. Non-UUID values are replaced: event envelopes require a UUID.
func CorrelationID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(authctx.HeaderCorrelationID)
		if _, err := uuid.Parse(id); err != nil {
			id = NewID()
		}
		r.Header.Set(authctx.HeaderCorrelationID, id)
		w.Header().Set(authctx.HeaderCorrelationID, id)
		next.ServeHTTP(w, r.WithContext(authctx.WithCorrelationID(r.Context(), id)))
	})
}

// NewID returns a time-ordered UUIDv7, falling back to v4 if the clock
// source fails.
func NewID() string {
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	return uuid.NewString()
}

// AccessLog emits one log line per request (docs/standards/logging-tracing.md
// §11) and names the request span after the matched route. Health checks
// log at DEBUG; 5xx at ERROR. It runs after service middleware so the log
// line carries the tenant and actor.
func AccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(sw, r)

		route := routePattern(r)
		span := trace.SpanFromContext(r.Context())
		span.SetName(r.Method + " " + route)
		span.SetAttributes(attribute.String("http.route", route))
		// The same keys StartSpan uses, so a correlation ID finds the trace.
		auth := telemetry.AuthAttrsFromContext(r.Context())
		for k, v := range map[string]string{
			"yc.correlation_id": auth.CorrelationID,
			"yc.tenant_id":      auth.TenantID,
			"yc.actor_type":     auth.ActorType,
		} {
			if v != "" {
				span.SetAttributes(attribute.String(k, v))
			}
		}

		level := slog.LevelInfo
		switch {
		case isHealthPath(r.URL.Path):
			level = slog.LevelDebug
		case sw.status >= http.StatusInternalServerError:
			level = slog.LevelError
		}
		ctx := r.Context()
		telemetry.LoggerFromContext(ctx).Log(ctx, level, "request",
			slog.String("http.method", r.Method),
			slog.String("http.route", route),
			slog.Int("http.status", sw.status),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
		)
	})
}

// Recoverer turns a panic into a logged 500. Code should not panic; this is
// defence in depth (docs/standards/error-handling.md §9).
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer recoverPanic(w, r)
		next.ServeHTTP(w, r)
	})
}

func recoverPanic(w http.ResponseWriter, r *http.Request) {
	rec := recover()
	if rec == nil {
		return
	}
	if rec == http.ErrAbortHandler { //nolint:errorlint // sentinel value, never wrapped
		panic(rec)
	}
	telemetry.LoggerFromContext(r.Context()).Error("panic recovered",
		slog.Any("panic", rec),
		slog.String("stack", string(debug.Stack())),
	)
	ycerrors.WriteError(w, r, ycerrors.Internal(fmt.Sprintf("panic: %v", rec)))
}

func routePattern(r *http.Request) string {
	if rc := chi.RouteContext(r.Context()); rc != nil {
		if p := rc.RoutePattern(); p != "" {
			return p
		}
	}
	return "unmatched"
}

type statusWriter struct {
	http.ResponseWriter
	status  int
	written bool
}

func (w *statusWriter) WriteHeader(status int) {
	if !w.written {
		w.status = status
		w.written = true
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.written = true
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach Flush and friends.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
