package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// StartSpan starts a span on the global tracer. It is a convenience over
// Tracer().Start that:
//
//   - Automatically enriches the span with auth attributes (tenant, actor)
//     from the context when present.
//   - Follows the yc-platform span naming convention: `<domain>.<operation>`
//     in lowercase. The caller is responsible for choosing the name.
//
// Example:
//
//	ctx, span := telemetry.StartSpan(ctx, "pricing.evaluate",
//	    attribute.String("sku_id", skuID),
//	)
//	defer span.End()
//
//	result, err := doWork(ctx)
//	if err != nil {
//	    telemetry.RecordSpanError(span, err)
//	    return nil, err
//	}
//	return result, nil
func StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	ctx, span := Tracer().Start(ctx, name)
	if len(attrs) > 0 {
		span.SetAttributes(attrs...)
	}

	auth := AuthAttrsFromContext(ctx)
	addIfSet := func(key, value string) {
		if value != "" {
			span.SetAttributes(attribute.String(key, value))
		}
	}
	addIfSet("yc.tenant_id", auth.TenantID)
	addIfSet("yc.actor_id", auth.ActorID)
	addIfSet("yc.actor_type", auth.ActorType)
	addIfSet("yc.correlation_id", auth.CorrelationID)

	return ctx, span
}

// RecordSpanError marks the span as errored and attaches the error. The
// error message is recorded on the span but not emitted as a log — logging
// is the caller's responsibility, typically at the transport boundary.
func RecordSpanError(span trace.Span, err error) {
	if err == nil || span == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// SpanContextIDs returns the trace and span IDs from the context, or empty
// strings if no valid span is active. Primarily for test assertions and
// correlation-ID propagation into event envelopes.
func SpanContextIDs(ctx context.Context) (traceID, spanID string) {
	span := trace.SpanFromContext(ctx)
	sc := span.SpanContext()
	if !sc.IsValid() {
		return "", ""
	}
	return sc.TraceID().String(), sc.SpanID().String()
}

// SpanNameFromParts composes a span name from parts, enforcing the
// yc-platform convention of lowercase dotted segments. Panics on empty
// input — a nameless span is a bug.
func SpanNameFromParts(parts ...string) string {
	if len(parts) == 0 {
		panic("telemetry: SpanNameFromParts called with no parts")
	}
	// Trivial for now; could lowercase/validate here.
	out := parts[0]
	for i := 1; i < len(parts); i++ {
		out = fmt.Sprintf("%s.%s", out, parts[i])
	}
	return out
}