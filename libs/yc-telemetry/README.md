# yc-telemetry

Logging, tracing, and metrics for yc-platform services.

## What it does

- Configures `log/slog` with a JSON handler that automatically enriches
  every log entry with:
  - Service metadata: `service`, `version`, `env`
  - Trace context: `trace_id`, `span_id`
  - Auth context: `tenant_id`, `actor_id`, `actor_type`, `correlation_id`
- Configures OpenTelemetry tracing with OTLP HTTP export
- Configures OpenTelemetry metrics with OTLP HTTP export
- Caches commonly used metric instruments so services declare them once

## Usage

```
package main

import (
    "context"
    "os"

    "github.com/slaghuis/yc-platform/libs/yc-telemetry"
)

func main() {
    ctx := context.Background()
    shutdown, err := telemetry.Init(ctx, telemetry.Config{
        ServiceName:    "yc-identity",
        ServiceVersion: "v1.2.3",
        Environment:    os.Getenv("YC_ENV"),
        OTLPEndpoint:   os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
        LogLevel:       os.Getenv("YC_LOG_LEVEL"),
    })
    if err != nil {
        panic(err)
    }
    defer shutdown(context.Background())

    // ...rest of main...
}
```
Elsewhere in the service:
```
func (s *Service) Register(ctx context.Context, email string) error {
    ctx, span := telemetry.StartSpan(ctx, "identity.register")
    defer span.End()

    logger := telemetry.LoggerFromContext(ctx)
    logger.Info("registering user", slog.String("email_domain", domain(email)))

    telemetry.Int64Counter("identity_registrations_total",
        "Count of user registrations").Add(ctx, 1)

    if err := s.repo.Save(ctx, user); err != nil {
        telemetry.RecordSpanError(span, err)
        return err
    }
    return nil
}
```
## Testing

```
func TestFoo(t *testing.T) {
    buf := telemetry.InitForTest(t, "yc-test")

    doSomething(context.Background())

    assert.Contains(t, buf.String(), "did something")
}
```
See telemetry_test.go for more.
###Conventions
See `docs/standards/logging-tracing.md` for the full conventions. The short version:
No PII in logs or trace attributes. See the PII rules in that document.
Span names: `<domain>.<operation>` lowercase, dotted.
Metric names: `<service_short>.<metric>` lowercase, snake_case.
Log messages: short, lowercase, present tense.

---

## What this delivers against the standards

Checking against `docs/standards/logging-tracing.md`:

| Standard | Covered |
|---|---|
| `slog` with JSON handler to stdout | ✓ |
| Service, version, env on every line | ✓ (via `contextHandler`) |
| `correlation_id`, `trace_id`, `span_id` auto-enriched | ✓ |
| `tenant_id`, `actor_id`, `actor_type` auto-enriched | ✓ |
| OTLP exporter configurable | ✓ (traces + metrics) |
| `StartSpan` convenience | ✓ |
| `RecordError` convenience | ✓ |
| `/healthz` / `/readyz` endpoints | ✗ (lives in `yc-httpkit`) |
| Access logging middleware | ✗ (lives in `yc-httpkit`) |
| Standard HTTP metrics | ✗ (lives in `yc-httpkit`) |
| Metric instrument caching | ✓ |

The absent items belong in `yc-httpkit`, where they can be plugged into chi middleware. That split keeps `yc-telemetry` free of HTTP concerns.

## What's deliberately out of scope

1. **A separate OpenTelemetry log bridge.** I considered wiring `otel/sdk/log` so logs flow through OTLP alongside traces and metrics. For v1, stdout JSON is simpler, matches our dev stack (docker compose captures stdout), and avoids a second ingestion path. If we later need log ingestion into the same backend as traces, we add it then — `contextHandler` is already decoupled from the writer.

2. **An in-memory tracer provider toggle in `Config`.** The current test setup works around this with its own `TracerProvider`. If testing span attributes becomes common, we add `WithTracerProvider(tp)` to `Config` so tests can inject a recording provider.

3. **Dynamic log level changes.** Services currently fix log level at startup. If we need runtime overrides (e.g., a debug endpoint for a single tenant), we add an atomic level pointer later.

4. **Metrics for the library itself.** The library doesn't emit meta-metrics about its own behaviour. The OTel SDKs do this internally; we rely on that.

## Three questions before moving on

1. **OpenTelemetry package versions**: I pinned to the current stable minor versions. OTel's Go SDK has had API churn, and the `log` SDK is still in `v0.x`. Comfortable with the choice to accept `v0.x` for the log bridge dependency? I kept the log bridge out of the critical path precisely because of this — the only `v0.x` dependency I kept is a transitive one via another module we're not using yet. If you prefer, I can drop the log-related imports from `go.mod` entirely until v1 is reached.

2. **Metric instrument caching strategy**: I cached by name only, with the description applied on first creation. Alternative: force services to declare instruments in an `app.Instruments` struct at startup, which is more explicit but more boilerplate. Current approach matches the `telemetry.Int64Counter("name", "desc").Add(ctx, 1)` idiom used in the standards doc. Keep as is?

3. **The `AuthKey` placement**: I defined `AuthKey` and `AuthAttrs` here rather than in `yc-authctx` to avoid an import cycle (both libraries need to agree on the context key). The alternative is a tiny shared package `libs/yc-context` just for the key, which some would consider cleaner. The current approach works but means `yc-authctx` will depend on `yc-telemetry` for the key definition. Preference?


