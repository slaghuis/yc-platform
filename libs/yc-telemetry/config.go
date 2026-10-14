package telemetry

import (
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Config controls telemetry initialization.
type Config struct {
	// ServiceName is the OpenTelemetry service.name resource attribute.
	// Required.
	ServiceName string

	// ServiceVersion is the OpenTelemetry service.version resource
	// attribute. Defaults to "dev".
	ServiceVersion string

	// Environment identifies the deployment (dev, staging, prod).
	// Defaults to "dev".
	Environment string

	// OTLPEndpoint is the OTLP HTTP endpoint for traces, metrics, and
	// logs. For example: http://localhost:4318. If empty, no exporter is
	// configured and telemetry stays in-process (useful for tests).
	OTLPEndpoint string

	// LogLevel for the slog logger. One of debug, info, warn, error.
	// Defaults to info.
	LogLevel string

	// LogWriter overrides the destination for log output. Defaults to
	// os.Stdout. Primarily for tests.
	LogWriter LogWriter

	// TraceSampleRatio controls head-based trace sampling (0.0–1.0).
	// Defaults to 1.0 (sample every trace) which is appropriate for dev;
	// lower in production.
	TraceSampleRatio float64

	// MetricExportInterval controls how often metrics are exported.
	// Defaults to 30 seconds.
	MetricExportInterval time.Duration

	// AdditionalResourceAttributes are merged into the resource. Service
	// name and version are set from Config fields; do not duplicate them
	// here.
	AdditionalResourceAttributes map[string]string
}

// LogWriter is the destination for structured log output. os.Stdout
// satisfies this interface.
type LogWriter interface {
	Write(p []byte) (n int, err error)
}

func (c *Config) applyDefaults() {
	if c.ServiceVersion == "" {
		c.ServiceVersion = "dev"
	}
	if c.Environment == "" {
		c.Environment = "dev"
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.TraceSampleRatio == 0 {
		c.TraceSampleRatio = 1.0
	}
	if c.MetricExportInterval == 0 {
		c.MetricExportInterval = 30 * time.Second
	}
}

func (c *Config) validate() error {
	if c.ServiceName == "" {
		return fmt.Errorf("telemetry: ServiceName is required")
	}
	if c.TraceSampleRatio < 0 || c.TraceSampleRatio > 1 {
		return fmt.Errorf("telemetry: TraceSampleRatio must be in [0,1], got %f", c.TraceSampleRatio)
	}
	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("telemetry: LogLevel must be one of debug|info|warn|error, got %q", c.LogLevel)
	}
	return nil
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}