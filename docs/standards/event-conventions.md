# Event Conventions

How events are named, structured, published, consumed, and evolved across yc-platform. Supplements ADR-001 (tenant isolation), ADR-003 (DLQ), ADR-006 (outbox), ADR-007 (schemas), and ADR-008 (platform tenant).

## 1. Principles

1. **Events are facts.** They describe what happened. Not what should happen.
2. **Events are past tense.** `order.placed.v1`, not `order.place` or `order.placing`.
3. **Events are immutable.** Once published, never modified. Corrections are new events.
4. **Events are versioned.** Breaking change = new version. v1 and v2 coexist until all consumers migrate.
5. **Events carry enough context** to be processed without round-trips, but reference IDs for large data.
6. **Events are not RPCs.** Don't model request/response as event pairs when a sync call is clearer.
7. **Every event is tenant-scoped** (or `_platform`).

## 2. Event vs Command

Two taxonomies on the bus:

### Events — facts

- Past tense: `order.placed.v1`, `payment.captured.v1`, `inventory.stock.reserved.v1`.
- Published after a state change has occurred.
- Potentially many consumers; publisher doesn't know or care who.

### Commands — requests

- Imperative or request form: `inventory.reservation_requested.v1`, `payment.capture_requested.v1`, `orders.order_creation_requested.v1`.
- Have a specific intended recipient service.
- Carry an idempotency key.
- Elicit a response event (success or failure form).

Both use the same envelope and the same bus. The distinction is semantic. Commands are used in sagas (yc-checkout coordinates via commands). Events are used for notification and projection updates.

**Guideline**: if you'd expect the publisher to care about who received it, it's a command. If the publisher is announcing an outcome, it's an event.

## 3. Naming

### Subject structure

```
yc.<tenant_id>.<domain>.<entity>.<action>.v<version>
```

- `yc.` — platform prefix, fixed.
- `<tenant_id>` — tenant ID or `_platform`.
- `<domain>` — bounded context (identity, order, payment, pricing, …). Matches the owning service's suffix (yc-order → `order`).
- `<entity>` — the thing acted upon (user, product, sku, reservation, charge).
- `<action>` — what happened (placed, cancelled, received, requested).
- `v<version>` — schema version.

### Rules

- All lowercase, snake_case for multi-word fields.
- Hyphens never used; underscores for word separation.
- Past tense for events (`placed`, `received`, `shipped`).
- `_requested` suffix for commands (`capture_requested`, `reservation_requested`).
- Domains, entities, and actions appear in the taxonomy registry (`docs/event-taxonomy.md`); new ones require a PR.

### Examples

```
yc.shop-abc.order.order.placed.v1
yc.shop-abc.inventory.reservation.requested.v1   ← command
yc.shop-abc.inventory.stock.reserved.v1          ← resulting event
yc._platform.tenant.tenant.provisioned.v1
```

## 4. Envelope

Every event wraps its payload in the envelope (schema at `envelope/v1.json`):

```json
{
  "event_id": "...uuid...",
  "event_type": "order.placed.v1",
  "event_time": "2025-01-15T10:00:00Z",
  "tenant_id": "shop-abc",
  "actor": {
    "type": "customer",
    "id": "user-uuid",
    "access_token_jti": "token-uuid"
  },
  "correlation_id": "...uuid...",
  "causation_id": "...uuid...",
  "source_service": "yc-orders",
  "schema_version": 1,
  "payload": { ... }
}
```

- `event_id`: UUID, unique per event. Used for consumer-side idempotency.
- `event_type`: matches the schema's identifier.
- `event_time`: UTC, millisecond precision.
- `tenant_id`: always present; `_platform` for platform events.
- `actor`: who caused this event (user or service).
- `correlation_id`: shared across all events in one logical operation.
- `causation_id`: event_id of the direct parent event, if any; null for user-triggered events.
- `source_service`: emitting service name.
- `schema_version`: integer matching the version in `event_type`.
- `payload`: the event-specific data; must match the schema.

Services never construct envelopes manually. `yc-eventbus.Publish` builds the envelope.

## 5. Payload Design

### Keep payloads small but complete

- **Include IDs** for related entities, not full objects.
- **Include enough denormalized context** to make the event useful without a round-trip. Example: `order.placed.v1` carries `user_id` and `tenant_id`, but also `customer_email` snapshot for yc-message to send a confirmation without calling back to yc-identity.
- **Don't dump entire aggregates.** If a consumer needs the full order, it can call yc-orders. The event carries only what common consumers need.

### Snapshots vs references

- **Snapshot** when the value at the time of the event matters (e.g., `order.placed.v1` snapshots `line_items` with prices at that moment — those prices are historical facts).
- **Reference** when consumers want current state (e.g., `product.product.updated.v1` carries product ID; consumers fetch the current product if they need details).

### Required vs optional fields

- **Required** fields are those every consumer needs. If any one consumer needs it, add it.
- **Optional** fields may be absent; consumers must handle their absence.
- **New optional fields** are backward compatible; new required fields are not.

## 6. Versioning

### When to version up

Create `v2` when:
- Removing a field.
- Renaming a field.
- Changing a field's type.
- Changing the meaning of a field.

Do **not** version up for:
- Adding an optional field.
- Adding a new value to an enum (consumers must handle unknowns anyway).
- Documentation changes.

### Migration process

1. Create new schema `v2.json` with the new structure.
2. Producers emit **both** `v1` and `v2` for a transition period.
3. Consumers migrate from v1 to v2 at their own pace.
4. Once all consumers moved: producers stop emitting v1.
5. v1 schema remains in source control forever (events published under v1 still exist).

Transition period minimum: 30 days.

### Deprecation

A schema file is marked deprecated by adding `"deprecated": true` and `"replaced_by": "v2"` to the schema. CI flags production of a deprecated version with a warning.

## 7. Publishing

Always through `yc-eventbus.Publish` within a database transaction (ADR-006, outbox pattern):

```go
err := tx.ExecContext(ctx, `UPDATE users SET ... WHERE id=$1`, userID)
if err != nil { return err }

err = eventbus.Publish(ctx, tx, eventbus.Event{
    EventType:     events.TypeIdentityUserPromotedV1,
    TenantID:      tenantID,
    Actor:         actor,
    CorrelationID: correlationID,
    CausationID:   causationID,
    Payload: events.IdentityUserPromotedV1{
        UserID:   userID,
        FromRole: "customer",
        ToRole:   "shop_admin",
    },
})
if err != nil { return err }

return tx.Commit()
```

Rules:
- **In the same transaction** as the state change. The outbox table lives in the service's DB.
- **Never publish from a non-transactional path.** If you don't have state to change, you probably don't have an event to publish.
- **Payload is a typed struct** from `libs/yc-events`, generated from the schema.
- **Correlation and causation IDs** propagate from the triggering operation (HTTP request or consumed event).

## 8. Consuming

Always through `yc-eventbus.Subscribe`:

```go
err := bus.Subscribe(ctx, eventbus.SubscribeOpts{
    Subject:     "yc.*.order.order.placed.v1",   // all tenants
    DurableName: "yc-message-order-placed",
    MaxRetries:  5,
    Concurrency: 4,
}, handleOrderPlaced)

func handleOrderPlaced(ctx context.Context, evt eventbus.ConsumedEvent) error {
    var p events.OrderPlacedV1
    if err := json.Unmarshal(evt.Payload, &p); err != nil {
        return fmt.Errorf("%w: unmarshal: %v", eventbus.ErrPoison, err)
    }
    return sendOrderConfirmation(ctx, p)
}
```

Rules:
- **Durable name = service + handler.** Enables consumer groups and resumability.
- **Idempotency is automatic** via the eventbus consumer-side dedup table — handlers can assume at-least-once but not exactly-once; duplicates are suppressed.
- **Non-nil error = retry** per backoff policy, then DLQ.
- **`ErrPoison` = immediate DLQ.**
- **Handlers are small**: unmarshal, delegate to a service method, return. Business logic lives in `internal/service/`, not in `internal/transport/events/`.

## 9. Tenant Filtering

Subscribers can filter at the subject level:

- `yc.*.order.order.placed.v1` — all tenants
- `yc.shop-abc.order.order.placed.v1` — one tenant
- `yc._platform.>` — all platform events

For most services, consume `yc.*.<domain>...` and let subject hierarchy do the work. Within a handler, the `envelope.TenantID` is available for business logic.

## 10. Idempotency

### Consumer-side

`yc-eventbus` dedupes by `event_id` using the `processed_events` table. Handlers can assume a given `event_id` is processed at most once per consumer group.

### Command idempotency keys

Commands (which cause state changes in a target service) carry an **idempotency key** in the payload:

```json
{
  "idempotency_key": "{session_id}:reserve",
  "items": [...]
}
```

The receiving service stores processed idempotency keys and returns the previous result on duplicate. This protects against:
- Outbox publishing twice.
- Service crash between publish and state update.
- Manual DLQ replay.

Keys are deterministic strings based on the saga and step. Not UUIDs. Not opaque. The receiving service must be able to detect "I've already done this specific thing."

## 11. Ordering

- **No cross-stream ordering guarantees.** Events from different subjects may arrive in any order.
- **Within a subject, NATS JetStream provides ordering** but consumers should not rely on it. Design handlers to be order-independent where possible.
- **For strictly ordered scenarios** (rare): use a single-partition consumer or model the ordering requirement differently (e.g., version numbers on the entity).

## 12. Fan-Out

Many events have multiple consumers. Example: `order.placed.v1` is consumed by yc-message, yc-shipping, yc-inventory, yc-analytics, yc-audit, yc-product.

- **Each consumer has its own durable name** → each gets its own copy.
- **Slow consumers do not block others** — NATS JetStream handles per-consumer offsets.
- **A failed consumer sends to its own DLQ**; other consumers are unaffected.

## 13. DLQ

Per ADR-003:

- Dead letter subject: `yc.<tenant_id>.<domain>.dlq.v1`.
- Original envelope is preserved; wrapped with `dlq_metadata`.
- yc-monitor subscribes to `yc.*.*.dlq.v1` and raises alerts.
- Replay via `yc-dlq-cli replay --subject ... --event-id ...`.
- A message that lands in DLQ three times (across replays) escalates to a `critical` alert — this indicates a systemic bug that replay alone cannot fix.

## 14. Correlation and Causation

Every event carries two trace IDs:

- **`correlation_id`**: shared across every event in a logical operation. If a customer clicks "place order" and that triggers 15 events across services, all 15 share the same `correlation_id`.
- **`causation_id`**: the `event_id` of the direct parent event. Null for events triggered by a user action (HTTP request). Set for events triggered by consuming another event.

### Propagation rules

- **HTTP request → first event**: `correlation_id` from `X-YC-Correlation-ID` (or generated), `causation_id` null.
- **Event consumed → new event emitted**: `correlation_id` from the consumed event, `causation_id` = consumed event's `event_id`.
- **Fan-out**: all child events share the parent's `correlation_id`, each with the parent's `event_id` as `causation_id`.

This lets us reconstruct the full tree of effects from a single user action.

## 15. Actor

The `actor` field identifies who caused the event. Four actor types:

```json
{ "type": "visitor",       "id": "<visitor-token-sub>", "access_token_jti": "..." }
{ "type": "customer",      "id": "<user-id>",           "access_token_jti": "..." }
{ "type": "shop_admin",    "id": "<user-id>",           "access_token_jti": "..." }
{ "type": "system_admin",  "id": "<user-id>",           "access_token_jti": "..." }
{ "type": "service",       "id": "<service-name>",      "access_token_jti": null }
```

- **Human actors** carry the `access_token_jti` for audit linkage.
- **Service actors** are used when the event is triggered by internal logic (scheduled jobs, cascades, startup migrations) rather than a human action.
- **Guest checkouts**: actor is `visitor` even after guest contact is captured; the user is not registered.

## 16. Event Catalog

Every event used in the platform is listed in `docs/event-taxonomy.md`:

- Event type (e.g., `order.placed.v1`)
- Producer service
- Consumer services
- Short description
- Trigger (what causes it)
- Compensation (if any — for commands)

**Adding a new event requires updating the catalog.** CI validates that every schema file has a catalog entry.

## 17. Common Field Conventions

Field naming in event payloads follows the same rules as HTTP APIs (see `api-conventions.md` §15):

- `snake_case`
- IDs: `<thing>_id` (e.g., `user_id`, `order_id`)
- Timestamps: `<thing>_at` (e.g., `placed_at`, `expires_at`)
- Durations: explicit units (e.g., `ttl_minutes`)
- Counts: `<thing>_count`
- Booleans: predicate form (`is_active`, `requires_shipping`)
- Money: `amount_minor` + `currency`

Common reusable payload fragments live in `schemas/yc-event-schemas/common/` and are referenced via `$ref`:

- `common/actor.v1.json`
- `common/money.v1.json`
- `common/address.v1.json`
- `common/line_item.v1.json`
- `common/guest_contact.v1.json`

## 18. PII in Events

- **Minimize PII in payloads.** Prefer IDs; let consumers fetch PII from the owning service when they truly need it.
- **PII is unavoidable for some events**: `identity.user.registered.v1` must carry an email so yc-message can send a welcome message without calling yc-identity.
- **PII-bearing events are subject to retention policy** (ADR-004) and GDPR cascade (ADR-005).
- **Never put secrets in events**: no passwords, no gateway tokens, no API keys. Reset flows carry one-time tokens; those are acceptable in the event (short-lived, single-use).

## 19. Testing Events

Every service that produces an event:
- **Unit test** that invoking the service method produces the correct event in the outbox.
- **Integration test** that the event is published to NATS and matches the schema.

Every service that consumes an event:
- **Unit test** of the handler with a fabricated event.
- **Integration test** that a published event triggers the expected side effect.

**Contract tests** are automatic via schema validation in `yc-eventbus`.

See `testing.md` for details.

## 20. Operational Rules

- **Replaying historical events is dangerous.** Consumers must be idempotent, but a replay may trigger side effects (emails sent, charges attempted). Replay only through `yc-dlq-cli` with human review.
- **Changing an event handler** retroactively applies to all future events, including replays. Document when a handler's behavior changes meaningfully.
- **Removing a consumer**: delete the durable consumer in NATS so messages aren't accumulated forever. Checklist is in `docs/runbooks/removing-a-service.md`.
- **Adding a new consumer to an existing event stream**: by default, starts from "now." If backfill is needed (e.g., a new projection), explicitly configure `deliver_all` with awareness of the volume.

---
