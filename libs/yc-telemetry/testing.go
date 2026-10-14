package telemetry

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

// InitForTest configures telemetry for tests with:
//   - a thread-safe in-memory LogWriter accessible via LogBuffer()
//   - no OTLP exporter (telemetry stays in-process)
//   - 100% trace sampling
//
// It resets the package singletons so each test gets a clean slate.
// Register as a cleanup with t.Cleanup.
func InitForTest(tb testing.TB, serviceName string) *Buffer {
	tb.Helper()

	buf := &Buffer{}
	resetSingletonsForTest()
	resetInstrumentCacheForTest()

	shutdown, err := Init(context.Background(), Config{
		ServiceName:      serviceName,
		ServiceVersion:   "test",
		Environment:      "test",
		OTLPEndpoint:     "", // in-process only
		LogLevel:         "debug",
		LogWriter:        buf,
		TraceSampleRatio: 1.0,
	})
	if err != nil {
		tb.Fatalf("telemetry.InitForTest: %v", err)
	}
	tb.Cleanup(func() {
		_ = shutdown(context.Background())
		resetSingletonsForTest()
		resetInstrumentCacheForTest()
	})

	return buf
}

// Buffer is a thread-safe writer that captures log output for test
// assertions.
type Buffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns the accumulated log output.
func (b *Buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Lines returns the accumulated log lines (JSON lines).
func (b *Buffer) Lines() []string {
	s := b.String()
	if s == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// Reset clears the buffer.
func (b *Buffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

// resetSingletonsForTest wipes the package singletons so tests can call
// Init repeatedly. Not exported.
func resetSingletonsForTest() {
	initOnce = sync.Once{}
	initErr = nil
	serviceName = atomic.Value{}
	globalLogger = atomic.Value{}
	globalTracer = atomic.Value{}
	globalMeter = atomic.Value{}
}