// Package telemetry configures logging (slog), tracing (OpenTelemetry), and
// metrics for every service. Services call Init at startup; everything else
// is automatic.
package telemetry