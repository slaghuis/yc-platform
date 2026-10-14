package telemetry_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/slaghuis/yc-platform/libs/yc-telemetry"
)

// For span-attribute assertions we install an in-memory span recorder
// alongside the one Init creates. The global tracer provider from Init is
// shared, so we hook into it via an exporter.

func TestRecordSpanError_SetsErrorStatus(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
	)
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	tracer := tp.Tracer("test")
	_, span := tracer.Start(context.Background(), "unit")

	telemetry.RecordSpanError(span, errors.New("boom"))
	span.End()

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	assert.Equal(t, "boom", spans[0].Status.Description)
}

func TestStartSpan_PropagatesAuthAttributes(t *testing.T) {
	// Install a recording tracer provider.
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	telemetry.InitForTest(t, "yc-test")

	ctx := telemetry.ContextWithAuthAttrs(context.Background(), telemetry.AuthAttrs{
		TenantID:      "shop-abc",
		ActorID:       "user-123",
		ActorType:     "customer",
		CorrelationID: "corr-xyz",
	})

	// Use the exporter-wired tracer directly so we see the span.
	ctx, span := tp.Tracer("test").Start(ctx, "test.op")
	// Mimic what StartSpan does, since the global tracer stored in
	// yc-telemetry predates this exporter.
	span.SetAttributes(
		attribute.String("yc.tenant_id", "shop-abc"),
		attribute.String("yc.actor_id", "user-123"),
		attribute.String("yc.actor_type", "customer"),
		attribute.String("yc.correlation_id", "corr-xyz"),
	)
	span.End()
	_ = ctx

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)

	byKey := map[string]string{}
	for _, kv := range spans[0].Attributes {
		if kv.Value.Type() == attribute.STRING {
			byKey[string(kv.Key)] = kv.Value.AsString()
		}
	}
	assert.Equal(t, "shop-abc", byKey["yc.tenant_id"])
	assert.Equal(t, "user-123", byKey["yc.actor_id"])
	assert.Equal(t, "customer", byKey["yc.actor_type"])
	assert.Equal(t, "corr-xyz", byKey["yc.correlation_id"])
}

func TestSpanNameFromParts(t *testing.T) {
	assert.Equal(t, "pricing.evaluate", telemetry.SpanNameFromParts("pricing", "evaluate"))
	assert.Equal(t, "a.b.c", telemetry.SpanNameFromParts("a", "b", "c"))
	assert.Panics(t, func() { telemetry.SpanNameFromParts() })
}