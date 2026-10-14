## Local Services ##
Once the dev stack is up:
| Service | URL |
| ------- | --- |
| Postgres | postgres://postgres:devpass@localhost:5432 |
| NATS | nats://localhost:4222 |
| NATS admin | http://localhost:8222 |
| MinIO | http://localhost:9001 (devkey / devsecret) |
| Jaeger | http://localhost:16686 |
| MailHog | http://localhost:8025 |

## Troubleshooting
*Port conflicts* If port 5432 (or any other) is in use, copy docker-compose.override.yml.example to docker-compose.override.yml and remap the port.
*Dev stack won't start*
`
task devstack:reset  # wipes volumes and restarts
`
*Pre-commit failures*
`
pre-commit run --all-files  # see what's failing
`
*Go workspace issues*
`
### `docs/onboarding/adding-a-service.md`

```markdown
# Adding a Service

## 1. Scaffold

```bash
task scaffold:service NAME=yc-foo
`
This creates 'services/yc-foo/' with the standard directory layout, `go.mod`, `main.go`, `Taskfile.yml`, migrations for the outbox tables, and a Dockerfile symlink.

## 2. Wire Into the Monorepo
 - [] Add `./services/yc-foo` to go.work
 - []  Add `yc_foo` to `deploy/compose/postgres-init.sh`
 - []  Add `YC_FOO_DB_URL` to `.env.example`
 - []  Add `yc-foo` to CI matrices and workflows if needed
 - []  Create `docs/services/yc-foo.md` with the full requirement spec

## 3. Implement
Follow the structure in `docs/standards/go-conventions.md`:
`
services/yc-foo/
├── cmd/yc-foo/main.go           — entry point
├── internal/
│   ├── app/                      — composition root
│   ├── domain/                   — types, no I/O
│   ├── service/                  — use cases
│   ├── repository/               — persistence
│   └── transport/
│       ├── http/                 — handlers
│       └── events/               — subscribers
└── migrations/                   — schema evolution
`
Reference the yc-identity service as a worked example.
## 4. Expose Routes
Register with yc-gateway at startup (or add to the static route file for cold-start availability).
## 5. Test
 - Unit tests next to source
 - Integration tests under `tests/integration/` with build tag integration
 - Register the service in `test/e2e/` if it participates in cross-service flows
`
### `docs/onboarding/adding-an-event.md`

```markdown
# Adding an Event

## 1. Create the Schema

```bash
task scaffold:event DOMAIN=order ENTITY=order ACTION=priority_elevated VERSION=1
`
This creates `schemas/yc-event-schemas/order/order.priority_elevated.v1.json`.
Fill in the schema. Reference `common/` fragments where applicable. See `docs/standards/event-conventions.md` for naming and payload rules.

## 2.Regenerate Go Types
`
task gen:events
`
Generated types land in `libs/yc-events/generated/`.

## 3. Register in the Taxonomy
Add an entry in `docs/event-taxonomy.md`:
 - Event type
 - Producer service
 - Consumer services
 - Trigger
 - Compensation (for commands)
## 4. Publish
In the producer service, within a DB transaction:
`
err := eventbus.Publish(ctx, tx, eventbus.Event{
    EventType:     events.TypeOrderPriorityElevatedV1,
    TenantID:      tenantID,
    Actor:         actor,
    CorrelationID: correlationID,
    Payload:       events.OrderPriorityElevatedV1{ ... },
})
`
## 5. Consume
In each consumer service, subscribe:
`
bus.Subscribe(ctx, eventbus.SubscribeOpts{
    Subject:     "yc.*.order.order.priority_elevated.v1",
    DurableName: "yc-shipping-order-priority-elevated",
}, handleOrderPriorityElevated)
`
## 6. Test
 - Producer: unit test asserts the event lands in the outbox
 - Consumer: unit test with fabricated event; integration test with real NATS
`
### `docs/runbooks/README.md`

```markdown
# Runbooks

Step-by-step procedures for operational scenarios.

| Runbook | When to use |
|---|---|
| `dlq-replay.md`              | Messages are landing in a DLQ |
| `gdpr-incident-response.md`  | GDPR cascade incomplete after 72h |
| `onboarding-tenant.md`       | Provisioning a new tenant |
| `rotate-secrets.md`          | Rotating gateway credentials, JWT keys |
| `removing-a-service.md`      | Decommissioning a service |

Each runbook is written for operators who may not be the author. Assume
nothing; link to supporting docs.
`

`docs/runbooks/dlq-replay.md` *(stub)*

`
# DLQ Replay

## When to use

- yc-monitor raised an alert for a tenant/domain DLQ.
- You've investigated the failure and have a fix deployed (or confirmed the
  original message was poison).

## Procedure

1. Identify affected messages:
   ```bash
   yc-dlq-cli list --subject "yc.<tenant>.<domain>.dlq.v1"
`
2. Inspect one:
yc-dlq-cli show --event-id <id>

3. Replay (dry run first):
yc-dlq-cli replay --event-id <id> --dry-run
yc-dlq-cli replay --event-id <id>

4. Verify downstream effect in the consumer service.

### Full implementation lands with yc-dlq-cli in Phase 1.
`### `docs/runbooks/gdpr-incident-response.md` (stub)

## Full implimentationlands with yc-tenant-cli in Phase 2.
`
`