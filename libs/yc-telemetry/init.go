package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// ShutdownFunc flushes buffered telemetry and releases resources. Services
// defer it from main.
type ShutdownFunc func(ctx context.Context) error

var (
	initOnce     sync.Once
	initErr      error
	serviceName  atomic.Value // string
	globalLogger atomic.Value // *slog.Logger
	globalTracer atomic.Value // trace.Tracer
	globalMeter  atomic.Value // metric.Meter
	noopShutdown = func(context.Context) error { return nil }
)

// Init configures logging, tracing, and metrics for the calling service.
// It must be called once at startup, before any other package in the service
// emits telemetry. Subsequent calls are no-ops.
//
// The returned ShutdownFunc must be deferred in main to flush buffered
// telemetry on shutdown.
func Init(ctx context.Context, cfg Config) (ShutdownFunc, error) {
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return noopShutdown, err
	}

	var shutdown ShutdownFunc = noopShutdown
	initOnce.Do(func() {
		shutdown, initErr = doInit(ctx, cfg)
	})
	return shutdown, initErr
}

func doInit(ctx context.Context, cfg Config) (ShutdownFunc, error) {
	serviceName.Store(cfg.ServiceName)

	res, err := buildResource(cfg)
	if err != nil {
		return noopShutdown, fmt.Errorf("telemetry: build resource: %w", err)
	}

	var shutdownFns []ShutdownFunc

	// Traces
	tp, traceShutdown, err := setupTraces(ctx, cfg, res)
	if err != nil {
		return noopShutdown, fmt.Errorf("telemetry: setup traces: %w", err)
	}
	otel.SetTracerProvider(tp)
	// W3C trace context lets a trace span services: yc-httpkit's client
	// injects traceparent and its server middleware extracts it.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	globalTracer.Store(tp.Tracer(cfg.ServiceName))
	shutdownFns = append(shutdownFns, traceShutdown)

	// Metrics
	mp, metricShutdown, err := setupMetrics(ctx, cfg, res)
	if err != nil {
		return noopShutdown, fmt.Errorf("telemetry: setup metrics: %w", err)
	}
	otel.SetMeterProvider(mp)
	globalMeter.Store(mp.Meter(cfg.ServiceName))
	shutdownFns = append(shutdownFns, metricShutdown)

	// Logging — slog with a JSON handler, wrapped to add trace context.
	logger := newLogger(cfg)
	globalLogger.Store(logger)
	slog.SetDefault(logger)

	return composeShutdown(shutdownFns), nil
}

func buildResource(cfg Config) (*resource.Resource, error) {
	attrs := make([]attribute.KeyValue, 0, 3+len(cfg.AdditionalResourceAttributes))
	attrs = append(attrs,
		semconv.ServiceName(cfg.ServiceName),
		semconv.ServiceVersion(cfg.ServiceVersion),
		attribute.String("deployment.environment", cfg.Environment),
	)
	for k, v := range cfg.AdditionalResourceAttributes {
		attrs = append(attrs, attribute.String(k, v))
	}
	return resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(semconv.SchemaURL, attrs...),
	)
}

func setupTraces(ctx context.Context, cfg Config, res *resource.Resource) (*sdktrace.TracerProvider, ShutdownFunc, error) {
	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(
			sdktrace.TraceIDRatioBased(cfg.TraceSampleRatio),
		)),
	}

	if cfg.OTLPEndpoint != "" {
		exp, err := otlptracehttp.New(ctx,
			otlptracehttp.WithEndpointURL(cfg.OTLPEndpoint+"/v1/traces"),
			otlptracehttp.WithInsecure(),
		)
		if err != nil {
			return nil, noopShutdown, err
		}
		opts = append(opts, sdktrace.WithBatcher(exp))
	}

	tp := sdktrace.NewTracerProvider(opts...)
	return tp, tp.Shutdown, nil
}

func setupMetrics(ctx context.Context, cfg Config, res *resource.Resource) (*sdkmetric.MeterProvider, ShutdownFunc, error) {
	opts := []sdkmetric.Option{
		sdkmetric.WithResource(res),
	}

	// Jaeger, the local collector, accepts traces only; the standard
	// OTEL_METRICS_EXPORTER=none keeps metrics in-process.
	if cfg.OTLPEndpoint != "" && os.Getenv("OTEL_METRICS_EXPORTER") != "none" {
		exp, err := otlpmetrichttp.New(ctx,
			otlpmetrichttp.WithEndpointURL(cfg.OTLPEndpoint+"/v1/metrics"),
			otlpmetrichttp.WithInsecure(),
		)
		if err != nil {
			return nil, noopShutdown, err
		}
		opts = append(opts, sdkmetric.WithReader(
			sdkmetric.NewPeriodicReader(exp,
				sdkmetric.WithInterval(cfg.MetricExportInterval),
			),
		))
	}

	mp := sdkmetric.NewMeterProvider(opts...)
	return mp, mp.Shutdown, nil
}

func newLogger(cfg Config) *slog.Logger {
	var w LogWriter = os.Stdout
	if cfg.LogWriter != nil {
		w = cfg.LogWriter
	}

	base := slog.NewJSONHandler(&writerAdapter{w: w}, &slog.HandlerOptions{
		Level:     parseLevel(cfg.LogLevel),
		AddSource: false,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			// Rename "msg" -> "message" is tempting but we keep stdlib
			// convention for tooling compatibility.
			return a
		},
	})

	handler := &contextHandler{
		next:    base,
		service: cfg.ServiceName,
		version: cfg.ServiceVersion,
		env:     cfg.Environment,
	}

	return slog.New(handler)
}

func composeShutdown(fns []ShutdownFunc) ShutdownFunc {
	return func(ctx context.Context) error {
		var errs []error
		for _, fn := range fns {
			if err := fn(ctx); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
}

type writerAdapter struct {
	w LogWriter
}

func (a *writerAdapter) Write(p []byte) (int, error) {
	return a.w.Write(p)
}

// Tracer returns the global tracer for the calling service.
// Panics if Init has not been called.
func Tracer() trace.Tracer {
	v := globalTracer.Load()
	if v == nil {
		panic("telemetry: Tracer called before Init")
	}
	return v.(trace.Tracer)
}

// Meter returns the global meter for the calling service.
// Panics if Init has not been called.
func Meter() metric.Meter {
	v := globalMeter.Load()
	if v == nil {
		panic("telemetry: Meter called before Init")
	}
	return v.(metric.Meter)
}

// Logger returns the global logger for the calling service.
// Panics if Init has not been called.
func Logger() *slog.Logger {
	v := globalLogger.Load()
	if v == nil {
		panic("telemetry: Logger called before Init")
	}
	return v.(*slog.Logger)
}

// ServiceName returns the configured service name after Init.
func ServiceName() string {
	v := serviceName.Load()
	if v == nil {
		return ""
	}
	return v.(string)
}
