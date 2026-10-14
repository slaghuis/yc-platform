package telemetry

import (
	"fmt"
	"sync"

	"go.opentelemetry.io/otel/metric"
)

// Instruments wraps commonly used instrument types so services declare them
// once and ignore the metric.Meter interface. All instruments are cached
// by name so repeated calls return the same instrument.
type instrumentCache struct {
	mu          sync.Mutex
	int64Counts map[string]metric.Int64Counter
	int64Hists  map[string]metric.Int64Histogram
	float64Hist map[string]metric.Float64Histogram
	int64Gauges map[string]metric.Int64UpDownCounter
}

var cache = &instrumentCache{
	int64Counts: map[string]metric.Int64Counter{},
	int64Hists:  map[string]metric.Int64Histogram{},
	float64Hist: map[string]metric.Float64Histogram{},
	int64Gauges: map[string]metric.Int64UpDownCounter{},
}

// Int64Counter returns a cached Int64Counter. The description is applied
// on first creation and ignored on subsequent lookups.
func Int64Counter(name, description string) metric.Int64Counter {
	cache.mu.Lock()
	defer cache.mu.Unlock()

	if c, ok := cache.int64Counts[name]; ok {
		return c
	}
	c, err := Meter().Int64Counter(name, metric.WithDescription(description))
	if err != nil {
		panic(fmt.Sprintf("telemetry: Int64Counter(%q): %v", name, err))
	}
	cache.int64Counts[name] = c
	return c
}

// Int64Histogram returns a cached Int64Histogram. Use for discrete-unit
// distributions like byte sizes or item counts.
func Int64Histogram(name, description, unit string) metric.Int64Histogram {
	cache.mu.Lock()
	defer cache.mu.Unlock()

	if h, ok := cache.int64Hists[name]; ok {
		return h
	}
	h, err := Meter().Int64Histogram(name,
		metric.WithDescription(description),
		metric.WithUnit(unit),
	)
	if err != nil {
		panic(fmt.Sprintf("telemetry: Int64Histogram(%q): %v", name, err))
	}
	cache.int64Hists[name] = h
	return h
}

// Float64Histogram returns a cached Float64Histogram. Prefer for durations
// (unit "s") and other continuous quantities.
func Float64Histogram(name, description, unit string) metric.Float64Histogram {
	cache.mu.Lock()
	defer cache.mu.Unlock()

	if h, ok := cache.float64Hist[name]; ok {
		return h
	}
	h, err := Meter().Float64Histogram(name,
		metric.WithDescription(description),
		metric.WithUnit(unit),
	)
	if err != nil {
		panic(fmt.Sprintf("telemetry: Float64Histogram(%q): %v", name, err))
	}
	cache.float64Hist[name] = h
	return h
}

// Int64UpDownCounter returns a cached up/down counter. Use for gauges-like
// values that can rise and fall (e.g., outbox pending count).
func Int64UpDownCounter(name, description string) metric.Int64UpDownCounter {
	cache.mu.Lock()
	defer cache.mu.Unlock()

	if g, ok := cache.int64Gauges[name]; ok {
		return g
	}
	g, err := Meter().Int64UpDownCounter(name, metric.WithDescription(description))
	if err != nil {
		panic(fmt.Sprintf("telemetry: Int64UpDownCounter(%q): %v", name, err))
	}
	cache.int64Gauges[name] = g
	return g
}

// resetInstrumentCacheForTest clears the instrument cache. Only called
// from tests, not exported.
func resetInstrumentCacheForTest() {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.int64Counts = map[string]metric.Int64Counter{}
	cache.int64Hists = map[string]metric.Int64Histogram{}
	cache.float64Hist = map[string]metric.Float64Histogram{}
	cache.int64Gauges = map[string]metric.Int64UpDownCounter{}
}
