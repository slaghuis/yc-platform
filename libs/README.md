# Shared Libraries

Go modules shared across services. Each is its own module registered in
`go.work`.

| Library | Purpose |
|---|---|
| `yc-authctx`   | AuthContext propagation and HTTP middleware |
| `yc-config`    | YAML + env config loader |
| `yc-dbkit`     | Postgres helpers, migrations, RLS |
| `yc-errors`    | Error taxonomy and problem+json serialization |
| `yc-eventbus`  | NATS JetStream, outbox, DLQ, idempotency |
| `yc-events`    | Generated Go types from schemas |
| `yc-httpkit`   | HTTP server, middleware, graceful shutdown |
| `yc-money`     | Money type with minor-unit arithmetic |
| `yc-telemetry` | Logging, tracing, metrics setup |
| `yc-testkit`   | Shared test helpers |

Rules:
- Libraries never import services.
- Libraries may import each other only in the dependency order implied above
  (lower utilities don't import higher-level ones).
- Breaking changes require updates to all consuming services in the same PR
  (easy in a monorepo).
