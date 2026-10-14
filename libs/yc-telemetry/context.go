package telemetry

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// AuthKey is the context key under which yc-authctx stores auth attributes.
// Defined here to avoid an import cycle with yc-authctx; the structure is
// documented in docs/standards/logging-tracing.md.
type AuthKey struct{}

// AuthAttrs is the shape yc-telemetry expects when extracting auth context.
// yc-authctx.AuthContext satisfies this via a conversion in its own package.
type AuthAttrs struct {
	TenantID       string
	ActorID        string
	ActorType      string
	CorrelationID  string
	AccessTokenJTI string
}

// AuthAttrsFromContext extracts auth attributes if present. Returns the
// zero value when absent. The returned struct is read-only; callers must
// not mutate it.
func AuthAttrsFromContext(ctx context.Context) AuthAttrs {
	v := ctx.Value(AuthKey{})
	if v == nil {
		return AuthAttrs{}
	}
	a, ok := v.(AuthAttrs)
	if !ok {
		return AuthAttrs{}
	}
	return a
}

// ContextWithAuthAttrs attaches auth attributes to the context. yc-authctx
// uses this; services should go through yc-authctx rather than calling
// this directly.
func ContextWithAuthAttrs(ctx context.Context, a AuthAttrs) context.Context {
	return context.WithValue(ctx, AuthKey{}, a)
}

// LoggerFromContext returns a logger enriched with trace context (trace_id,
// span_id) and auth context (tenant_id, actor_id, actor_type,
// correlation_id) extracted from ctx. Use this in handlers and services.
//
// If Init has not been called, falls back to the stdlib default slog
// logger (which writes plain-text to stderr) rather than panicking —
// LoggerFromContext may be called from very early in startup.
func LoggerFromContext(ctx context.Context) *slog.Logger {
	base := loggerOrDefault()

	attrs := make([]slog.Attr, 0, 6)

	if span := trace.SpanFromContext(ctx); span.SpanContext().IsValid() {
		sc := span.SpanContext()
		attrs = append(attrs,
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}

	auth := AuthAttrsFromContext(ctx)
	if auth.CorrelationID != "" {
		attrs = append(attrs, slog.String("correlation_id", auth.CorrelationID))
	}
	if auth.TenantID != "" {
		attrs = append(attrs, slog.String("tenant_id", auth.TenantID))
	}
	if auth.ActorID != "" {
		attrs = append(attrs, slog.String("actor_id", auth.ActorID))
	}
	if auth.ActorType != "" {
		attrs = append(attrs, slog.String("actor_type", auth.ActorType))
	}

	if len(attrs) == 0 {
		return base
	}

	// slog.Logger.With accepts any, so expand slog.Attr into alternating
	// key/value pairs via the Attr-aware With form.
	anyAttrs := make([]any, 0, len(attrs))
	for _, a := range attrs {
		anyAttrs = append(anyAttrs, a)
	}
	return base.With(anyAttrs...)
}

func loggerOrDefault() *slog.Logger {
	v := globalLogger.Load()
	if v == nil {
		return slog.Default()
	}
	return v.(*slog.Logger)
}

// contextHandler wraps a slog.Handler and augments every record with
// trace and auth context extracted from the current context, plus static
// service metadata. This means direct calls like `slog.InfoContext(ctx, ...)`
// are also automatically enriched, not only `LoggerFromContext(ctx)`.
type contextHandler struct {
	next    slog.Handler
	service string
	version string
	env     string
}

func (h *contextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	// Static service attributes
	r.AddAttrs(
		slog.String("service", h.service),
		slog.String("version", h.version),
		slog.String("env", h.env),
	)

	// Trace context
	if span := trace.SpanFromContext(ctx); span.SpanContext().IsValid() {
		sc := span.SpanContext()
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}

	// Auth context
	auth := AuthAttrsFromContext(ctx)
	if auth.CorrelationID != "" {
		r.AddAttrs(slog.String("correlation_id", auth.CorrelationID))
	}
	if auth.TenantID != "" {
		r.AddAttrs(slog.String("tenant_id", auth.TenantID))
	}
	if auth.ActorID != "" {
		r.AddAttrs(slog.String("actor_id", auth.ActorID))
	}
	if auth.ActorType != "" {
		r.AddAttrs(slog.String("actor_type", auth.ActorType))
	}

	return h.next.Handle(ctx, r)
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &contextHandler{
		next:    h.next.WithAttrs(attrs),
		service: h.service,
		version: h.version,
		env:     h.env,
	}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{
		next:    h.next.WithGroup(name),
		service: h.service,
		version: h.version,
		env:     h.env,
	}
}