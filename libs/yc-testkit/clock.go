package testkit

import (
	"sync"
	"time"
)

// FakeClock is a controllable clock for tests. It satisfies any
// `interface{ Now() time.Time }` a service defines. The zero value starts at
// the Unix epoch; NewFakeClock picks the start.
type FakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewFakeClock returns a clock fixed at start (converted to UTC).
func NewFakeClock(start time.Time) *FakeClock {
	return &FakeClock{now: start.UTC()}
}

// Now returns the current fake time.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.now.IsZero() {
		return time.Unix(0, 0).UTC()
	}
	return c.now
}

// Advance moves the clock forward by d.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.now.IsZero() {
		c.now = time.Unix(0, 0).UTC()
	}
	c.now = c.now.Add(d)
}

// Set moves the clock to t.
func (c *FakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t.UTC()
}
