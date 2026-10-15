package domain

import (
	"fmt"
	"time"
)

// Counters yc-monitor keeps, rolled up per tenant.
const (
	MetricCheckoutsStarted   = "checkouts.started"
	MetricCheckoutsCompleted = "checkouts.completed"
	MetricCheckoutsFailed    = "checkouts.failed"
	MetricOrdersPlaced       = "orders.placed"
	MetricPaymentsFailed     = "payments.failed" // by gateway
	MetricDeadLetters        = "events.dlq"      // by service
)

// Metrics lists every counter.
var Metrics = []string{
	MetricCheckoutsStarted, MetricCheckoutsCompleted, MetricCheckoutsFailed,
	MetricOrdersPlaced, MetricPaymentsFailed, MetricDeadLetters,
}

// ValidMetric reports whether m is a counter.
func ValidMetric(m string) bool {
	for _, x := range Metrics {
		if x == m {
			return true
		}
	}
	return false
}

// Bucket is a rollup bucket size.
type Bucket string

// Bucket sizes.
const (
	Bucket1m Bucket = "1m"
	Bucket5m Bucket = "5m"
	Bucket1h Bucket = "1h"
	Bucket1d Bucket = "1d"
)

// Buckets lists every bucket size, smallest first.
var Buckets = []Bucket{Bucket1m, Bucket5m, Bucket1h, Bucket1d}

// Duration is the bucket's length.
func (b Bucket) Duration() time.Duration {
	switch b {
	case Bucket1m:
		return time.Minute
	case Bucket5m:
		return 5 * time.Minute
	case Bucket1h:
		return time.Hour
	case Bucket1d:
		return 24 * time.Hour
	}
	return 0
}

// Start is the start of the bucket holding t (UTC).
func (b Bucket) Start(t time.Time) time.Time {
	return t.UTC().Truncate(b.Duration())
}

// ParseBucket returns the bucket named s.
func ParseBucket(s string) (Bucket, error) {
	b := Bucket(s)
	if b.Duration() == 0 {
		return "", fmt.Errorf("bucket %q is not one of 1m, 5m, 1h, 1d", s)
	}
	return b, nil
}

// Count is one increment of a counter.
type Count struct {
	TenantID   string
	Metric     string
	Dimensions map[string]string
	At         time.Time
}

// Point is a counter's value in one bucket.
type Point struct {
	BucketStart time.Time `json:"bucket_start"`
	Value       int64     `json:"value"`
}

// MetricQuery selects a counter's series. An empty TenantID sums every
// tenant; Dimensions narrows to matching rows.
type MetricQuery struct {
	TenantID   string
	Metric     string
	Bucket     Bucket
	Dimensions map[string]string
	From, To   time.Time
}
