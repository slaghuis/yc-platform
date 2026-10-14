// Package telemetry configures logging (slog), tracing (OpenTelemetry), and
// metrics for every yc-platform service.
//
// Services call Init once at startup and ignore the rest:
//
//	shutdown, err := telemetry.Init(ctx, telemetry.Config{
//	    ServiceName:    "yc-identity",
//	    ServiceVersion: version,
//	    OTLPEndpoint:   "http://localhost:4318",
//	})
//	if err != nil { ... }
//	defer shutdown(context.Background())
//
// After Init, use the package-level helpers:
//
//	logger := telemetry.LoggerFromContext(ctx)
//	logger.Info("user registered", slog.String("user_id", id))
//
//	ctx, span := telemetry.StartSpan(ctx, "service.register")
//	defer span.End()
//
//	meter := telemetry.Meter()
//	counter, _ := meter.Int64Counter("users_registered_total")
//	counter.Add(ctx, 1)
//
// Log entries emitted through LoggerFromContext automatically carry the
// trace_id, span_id, and the auth context fields (tenant_id, actor_id,
// actor_type, correlation_id) when those are present on the context.
//
// See docs/standards/logging-tracing.md for conventions.
package telemetry
