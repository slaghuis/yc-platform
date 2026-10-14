# Logging and Tracing

Observability conventions. Every service produces structured logs, distributed traces, and metrics. The three are correlated by a shared `correlation_id` and the OpenTelemetry trace context.

## 1. Overview

- **Logs**: structured JSON via `slog`, emitted to stdout. Captured by the deployment platform.
- **Traces**: OpenTelemetry, OTLP exporter to Jaeger (dev) or a hosted backend (prod).
- **Metrics**: OpenTelemetry, Prometheus-compatible exporter.
- **Correlation ID**: a UUID that flows through every HTTP request, every event, every log line.
- **Trace ID**: OpenTelemetry's own identifier, automatically propagated.
- **Both correlation ID and trace ID appear on every log line.**

## 2. Setup

Every service `main.go` calls `yc-telemetry.Init` before anything else that logs or traces:

```go
shutdown, err := telemetry.Init(ctx, "yc-identity", telemetry.Config{
    OTLPEndpoint:    os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
    ServiceVersion:  version,
    Environment:     cfg.Env,
    LogLevel:        cfg.LogLevel,
    SampleRatio:     1.0,  // dev; lower in prod
})
if err != nil {
    log.Fatalf("telemetry init: %v", err)
}
defer shutdown(context.Background())
```

`Init`:
- Configures a global `slog.Logger` with a JSON handler.
- Configures the OpenTelemetry tracer provider with OTLP exporter.
- Configures the metric provider.
- Returns a shutdown function that flushes buffered telemetry.

## 3. Logger Access

- **Package-level `telemetry.Logger`** returns the global logger. OK for small utilities.
- **Preferred**: inject a logger into structs that need one. Easier to test.
- **Context logger**: `telemetry.LoggerFromContext(ctx)` returns a logger enriched with the request's correlation ID and trace ID. **Use this in handlers and services.**

```go
func (s *Service) Register(ctx context.Context, email string) error {
    logger := telemetry.LoggerFromContext(ctx)
    logger.Info("registering user", slog.String("email", email))
    ...
}
```

## 4. Log Format

JSON output, one object per line:

```json
{
  "time": "2025-01-15T10:00:00.123Z",
  "level": "INFO",
  "msg": "user registered",
  "service": "yc-identity",
  "version": "1.0.0",
  "env": "prod",
  "correlation_id": "a1b2c3...",
  "trace_id": "4bf92f3577b34da6a3ce929d0e0e4736",
  "span_id": "00f067aa0ba902b7",
  "tenant_id": "shop-abc",
  "actor_id": "user-uuid",
  "actor_type": "customer",
  "user_id": "user-uuid",
  "email": "user@example.com"
}
```

- `time` is UTC, millisecond precision.
- `level` is `DEBUG`, `INFO`, `WARN`, `ERROR`.
- `msg` is a short human message, present tense.
- Service metadata is injected automatically.
- Request context is injected automatically when using `LoggerFromContext`.
- Custom fields follow (snake_case).

## 5. Log Levels

| Level | Use |
|---|---|
| DEBUG | Dev/troubleshooting. Noisy. Not emitted in prod by default. |
| INFO | Normal operations worth recording. User logged in. Order placed. Service started. |
| WARN | Something unexpected but handled. Retrying upstream call. Fallback to secondary channel. |
| ERROR | Something failed. 5xx responses. Unhandled errors. Failing jobs. |

Rules of thumb:
- Each successful handler invocation does not log at INFO. The access log (middleware) covers that.
- Significant business events (user registered, order placed, promotion started) log at INFO.
- Avoid DEBUG in hot paths; it clutters.
- ERROR must be actionable — if there's nothing to do about it, it's WARN.

## 6. Log Messages

- **Short, lowercase except proper nouns.** `"user registered"`, not `"User Registered Successfully"`.
- **Present tense.** `"processing order"`, not `"processed order"` (unless genuinely past).
- **No formatting.** `logger.Info("user registered", slog.String("user_id", id))`, not `logger.Info(fmt.Sprintf(...))`.
- **No PII in messages**; PII goes in attributes if necessary at all. See section 10.

## 7. Attribute Conventions

Common attribute keys, consistent across services:

| Key | Type | Description |
|---|---|---|
| `tenant_id` | string | Tenant identifier |
| `actor_id` | string | Authenticated actor ID |
| `actor_type` | string | visitor, customer, shop_admin, system_admin |
| `correlation_id` | string | Request correlation ID |
| `trace_id` | string | OpenTelemetry trace ID |
| `span_id` | string | OpenTelemetry span ID |
| `event_id` | string | Event envelope ID |
| `event_type` | string | Event type (e.g., order.placed.v1) |
| `user_id` | string | User affected by the operation |
| `order_id` | string | Order affected |
| `session_id` | string | Checkout session |
| `sku_id` | string | SKU affected |
| `duration_ms` | int64 | Operation duration |
| `err` | error | Via `slog.Any("err", err)` |
| `http.method` | string | HTTP method |
| `http.path` | string | Route pattern (not raw path) |
| `http.status` | int | Response status |

Service-specific attributes follow the same naming conventions (snake_case). Define them once at the top of a service's package if used repeatedly.

## 8. Tracing

Every HTTP request has a trace. Every event handler has a span. Internal operations within a service create child spans when meaningful.

### HTTP server middleware

Automatically creates a span per request:

```
GET /products
  http.method=GET
  http.route=/products
  http.status_code=200
  yc.tenant_id=shop-abc
  yc.actor_type=visitor
  duration=12ms
```

### HTTP client

The HTTP client in `yc-httpkit` is instrumented. Outgoing calls appear as child spans with upstream service names.

### Event publishing and consuming

`yc-eventbus` creates:
- **Producer span** when publishing: `event.publish order.placed.v1`
- **Consumer span** when handling: `event.consume order.placed.v1`
- **Links** between them via trace propagation in the event envelope (not just W3C headers; `correlation_id` and `causation_id` in the envelope achieve the same end).

### Manual spans

Add spans around meaningful business operations:

```go
ctx, span := telemetry.StartSpan(ctx, "pricing.evaluate")
defer span.End()

span.SetAttributes(
    attribute.String("sku_id", skuID),
    attribute.Int("quantity", qty),
)

result, err := s.evaluate(ctx, ...)
if err != nil {
    span.RecordError(err)
    span.SetStatus(codes.Error, err.Error())
    return nil, err
}
return result, nil
```

Rules:
- **Span names**: `<domain>.<operation>`. Lowercase. `pricing.evaluate`, not `EvaluatePricing`.
- **Add attributes for business keys**, not for every variable.
- **Record errors** on the span when they occur.
- **Set span status** to Error for failures.

## 9. Trace Sampling

- **Dev**: 100% sampling.
- **Prod**: 10% head-based by default, with upgrades for:
  - Error traces: 100% (parent-based sampling with error rule)
  - High-value traces (checkouts, payments): 100%
  - Admin API calls: 100%

Configured in `yc-telemetry`; services opt into special sampling by naming spans with specific prefixes.

## 10. PII in Logs and Traces

- **No raw PII in logs**: email, phone, full name, address, card data, auth tokens, OAuth secrets.
- **IDs are OK**: user_id, tenant_id, order_id — these are internal identifiers.
- **Hashed or truncated PII is OK when necessary**: `email_hash: sha256(...)`, `email_domain: "example.com"`.
- **Never log request bodies for registration, login, payment, or any PII-carrying endpoint.** Access log middleware excludes bodies by default.
- **Trace attributes follow the same rules** as log attributes.
- **Error context may contain PII** inadvertently; `yc-errors` scrubs known PII fields before logging.

## 11. Access Logging

yc-httpkit's middleware emits one log entry per HTTP request at INFO level:

```json
{
  "msg": "request",
  "level": "INFO",
  "service": "yc-product",
  "http.method": "GET",
  "http.route": "/products",
  "http.status": 200,
  "duration_ms": 24,
  "tenant_id": "shop-abc",
  "actor_type": "visitor",
  "correlation_id": "...",
  "trace_id": "..."
}
```

- **Route, not path**: `/products/{id}`, not `/products/abc-123`. Avoids high-cardinality log aggregation issues.
- **No request body, no response body** by default.
- **5xx responses log at ERROR** instead of INFO.

## 12. Metrics

OpenTelemetry metrics exported to Prometheus. Standard metrics per service:

| Metric | Type | Dimensions |
|---|---|---|
| `http.server.requests_total` | counter | method, route, status, tenant |
| `http.server.request_duration_seconds` | histogram | method, route, tenant |
| `http.client.requests_total` | counter | target, status |
| `http.client.request_duration_seconds` | histogram | target |
| `event.publish.total` | counter | type, tenant |
| `event.consume.total` | counter | type, tenant, outcome (ok/retry/dlq) |
| `event.processing_duration_seconds` | histogram | type |
| `outbox.pending` | gauge | — |
| `outbox.publish_duration_seconds` | histogram | — |
| `db.query_duration_seconds` | histogram | operation |

Service-specific metrics live in the service and follow the pattern `<service_short>.<metric>`.

- **Low cardinality.** Dimensions with high cardinality (user_id, order_id) go into traces, not metrics.
- **Histogram buckets** use OpenTelemetry defaults unless business requirements dictate otherwise.

## 13. Health vs Readiness

- `/healthz` — alive. No dependencies checked. Fast.
- `/readyz` — ready to serve. DB, NATS, migrations verified. Can return 503 during startup or when a critical dependency is down.

yc-monitor polls `/readyz` every 15 seconds.

## 14. Correlation Across Services

- **yc-gateway generates `X-YC-Correlation-ID`** for every incoming request without one.
- **Services pass it through** on outbound HTTP calls.
- **yc-eventbus includes it** in every event envelope (`correlation_id` and `causation_id`).
- **Event handlers reconstruct context** so logs and traces for event processing carry the correlation ID of the original user action.

## 15. Local Debugging

- **Dev stack includes Jaeger** at http://localhost:16686.
- **Logs go to stdout**; `docker compose logs -f yc-identity` works out of the box.
- **Trace a specific request**: pass `X-YC-Correlation-ID: debug-<something>` and grep logs.

## 16. Production Readiness Checklist

Before a service goes to prod:
- Logs at INFO are reviewed; nothing noisy or redundant.
- No PII in any log line (reviewed with sample data).
- Traces cover the full happy path and error paths.
- Metrics have dashboards in yc-monitor.
- `/healthz` and `/readyz` tested with chaos (dependency down).
- Access log middleware excludes any new PII-carrying endpoints by default.

---
