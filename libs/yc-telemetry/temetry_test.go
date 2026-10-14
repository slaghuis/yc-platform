package telemetry_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/slaghuis/yc-platform/libs/yc-telemetry"
)

func TestInit_RequiresServiceName(t *testing.T) {
	_, err := telemetry.Init(context.Background(), telemetry.Config{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ServiceName")
}

func TestInit_RejectsInvalidSampleRatio(t *testing.T) {
	_, err := telemetry.Init(context.Background(), telemetry.Config{
		ServiceName:      "yc-test",
		TraceSampleRatio: 2.0,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TraceSampleRatio")
}

func TestLogger_IncludesServiceMetadata(t *testing.T) {
	buf := telemetry.InitForTest(t, "yc-test")

	telemetry.Logger().Info("hello")

	line := firstLine(t, buf.String())
	assert.Equal(t, "yc-test", line["service"])
	assert.Equal(t, "test", line["version"])
	assert.Equal(t, "test", line["env"])
	assert.Equal(t, "hello", line["msg"])
	assert.Equal(t, "INFO", line["level"])
}

func TestLoggerFromContext_IncludesAuthAndTraceContext(t *testing.T) {
	buf := telemetry.InitForTest(t, "yc-test")

	ctx := telemetry.ContextWithAuthAttrs(context.Background(), telemetry.AuthAttrs{
		TenantID:      "shop-abc",
		ActorID:       "user-123",
		ActorType:     "customer",
		CorrelationID: "corr-xyz",
	})

	ctx, span := telemetry.StartSpan(ctx, "test.operation")
	defer span.End()

	logger := telemetry.LoggerFromContext(ctx)
	logger.Info("doing work", slog.String("sku_id", "sku-42"))

	line := firstLine(t, buf.String())
	assert.Equal(t, "shop-abc", line["tenant_id"])
	assert.Equal(t, "user-123", line["actor_id"])
	assert.Equal(t, "customer", line["actor_type"])
	assert.Equal(t, "corr-xyz", line["correlation_id"])
	assert.Equal(t, "sku-42", line["sku_id"])
	assert.NotEmpty(t, line["trace_id"])
	assert.NotEmpty(t, line["span_id"])
}

func TestInfoContext_IsEnrichedWithoutLoggerFromContext(t *testing.T) {
	// Direct slog.InfoContext on the default logger should pick up trace
	// context too, because the contextHandler is set as default.
	buf := telemetry.InitForTest(t, "yc-test")

	ctx := telemetry.ContextWithAuthAttrs(context.Background(), telemetry.AuthAttrs{
		TenantID: "shop-abc",
	})
	ctx, span := telemetry.StartSpan(ctx, "test.operation")
	defer span.End()

	slog.InfoContext(ctx, "direct")

	line := firstLine(t, buf.String())
	assert.Equal(t, "shop-abc", line["tenant_id"])
	assert.NotEmpty(t, line["trace_id"])
}

func TestStartSpan_SetsAuthAttributes(t *testing.T) {
	telemetry.InitForTest(t, "yc-test")

	ctx := telemetry.ContextWithAuthAttrs(context.Background(), telemetry.AuthAttrs{
		TenantID:  "shop-abc",
		ActorID:   "user-123",
		ActorType: "customer",
	})

	ctx, span := telemetry.StartSpan(ctx, "test.operation")
	defer span.End()

	traceID, spanID := telemetry.SpanContextIDs(ctx)
	assert.NotEmpty(t, traceID)
	assert.NotEmpty(t, spanID)
	// We can't easily assert span attributes without an in-memory exporter;
	// a dedicated span-recorder test is in trace_test.go.
}

func TestSpanContextIDs_EmptyWhenNoSpan(t *testing.T) {
	telemetry.InitForTest(t, "yc-test")

	traceID, spanID := telemetry.SpanContextIDs(context.Background())
	assert.Empty(t, traceID)
	assert.Empty(t, spanID)
}

func TestLoggerFromContext_FallsBackWithoutInit(t *testing.T) {
	// Deliberately do NOT call InitForTest. LoggerFromContext must not
	// panic — early-startup code may call it before Init completes.
	logger := telemetry.LoggerFromContext(context.Background())
	require.NotNil(t, logger)
	logger.Info("should not panic") // smoke only
}

func TestInstrumentCache_ReturnsSameInstrument(t *testing.T) {
	telemetry.InitForTest(t, "yc-test")

	c1 := telemetry.Int64Counter("test.events", "test events")
	c2 := telemetry.Int64Counter("test.events", "ignored second description")

	// Both pointers refer to the same cached instrument.
	assert.Equal(t, c1, c2)
}

func TestInit_IsIdempotent(t *testing.T) {
	buf := telemetry.InitForTest(t, "yc-test-1")

	// Second Init call is a no-op; service name does not change because
	// Init uses sync.Once.
	_, err := telemetry.Init(context.Background(), telemetry.Config{
		ServiceName: "yc-test-2",
	})
	require.NoError(t, err)
	assert.Equal(t, "yc-test-1", telemetry.ServiceName())

	// Logger still works.
	telemetry.Logger().Info("post second init")
	assert.NotEmpty(t, buf.String())
}

func firstLine(t *testing.T, s string) map[string]any {
	t.Helper()
	s = strings.TrimSpace(s)
	require.NotEmpty(t, s, "no log output")
	// First JSON line only.
	idx := strings.IndexByte(s, '\n')
	if idx >= 0 {
		s = s[:idx]
	}
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(s), &m))
	return m
}