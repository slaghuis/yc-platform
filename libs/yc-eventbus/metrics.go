package eventbus

import (
	"go.opentelemetry.io/otel/metric"

	"github.com/<org>/yc-platform/libs/yc-telemetry"
)

// Named for consistency with docs/standards/logging-tracing.md §12.
const (
	metricNamePublishTotal     = "event.publish.total"
	metricNameConsumeTotal     = "event.consume.total"
	metricNameOutboxPending    = "outbox.pending"
	metricNamePublishDuration  = "event.publish_duration_seconds"
	metricNameConsumeDuration  = "event.processing_duration_seconds"
)

func (c *Client) initMetrics() {
	c.metricPublishTotal = telemetry.Int64Counter(
		metricNamePublishTotal,
		"Number of events published to NATS from the outbox",
	)
	c.metricConsumeTotal = telemetry.Int64Counter(
		metricNameConsumeTotal,
		"Number of events consumed, counted by outcome (ok/retry/dlq)",
	)
	c.metricOutboxPending = telemetry.Int64UpDownCounter(
		metricNameOutboxPending,
		"Current count of unpublished rows in the outbox table",
	)
	c.metricPublishDurationSec = telemetry.Float64Histogram(
		metricNamePublishDuration,
		"Time from outbox read to NATS publish, in seconds",
		"s",
	)
	c.metricConsumeDurationSec = telemetry.Float64Histogram(
		metricNameConsumeDuration,
		"Time to process a consumed event, in seconds",
		"s",
	)
}

// Typed accessors keep usage sites tidy.
func (c *Client) publishCounter() metric.Int64Counter {
	return c.metricPublishTotal.(metric.Int64Counter)
}
func (c *Client) consumeCounter() metric.Int64Counter {
	return c.metricConsumeTotal.(metric.Int64Counter)
}
func (c *Client) outboxPending() metric.Int64UpDownCounter {
	return c.metricOutboxPending.(metric.Int64UpDownCounter)
}
func (c *Client) publishDuration() metric.Float64Histogram {
	return c.metricPublishDurationSec.(metric.Float64Histogram)
}
func (c *Client) consumeDuration() metric.Float64Histogram {
	return c.metricConsumeDurationSec.(metric.Float64Histogram)
}