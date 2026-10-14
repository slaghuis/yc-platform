# Adding an Event

Events are facts. If the producer needs another service to do something and cares about the answer, make a synchronous internal call instead (`docs/standards/event-conventions.md` §2).

## 1. Create the Schema

```bash
task scaffold:event DOMAIN=order ACTION=priority_elevated VERSION=1
```

This creates `schemas/yc-event-schemas/order/order.priority_elevated.v1.json`. Add `ENTITY=...` when the event is about an entity other than the domain's own aggregate (for example `DOMAIN=inventory ENTITY=stock ACTION=reserved` creates `inventory/inventory.stock.reserved.v1.json`).

Fill in the schema. Reference `common/` fragments where applicable. See `docs/standards/event-conventions.md` for naming and payload rules.

## 2. Regenerate Go Types

```bash
task gen:events
```

Generated types land in `libs/yc-events/generated/`.

## 3. Register in the Taxonomy

Add an entry in `docs/diagrams/event-taxonomy.md`:
- Event type
- Producer service
- Consumer services
- Trigger

## 4. Publish

In the producer service, within a DB transaction:

```go
err := eventbus.Publish(ctx, tx, eventbus.Event{
    EventType:     events.TypeOrderPriorityElevatedV1,
    TenantID:      tenantID,
    Actor:         actor,
    CorrelationID: correlationID,
    Payload:       events.OrderPriorityElevatedV1{ ... },
})
```

## 5. Consume

In each consumer service, subscribe:

```go
bus.Subscribe(ctx, eventbus.SubscribeOpts{
    Subject:     "yc.*.order.priority_elevated.v1",
    DurableName: "yc-shipping-order-priority-elevated",
}, handleOrderPriorityElevated)
```

## 6. Test

- Producer: unit test asserts the event lands in the outbox
- Consumer: unit test with a fabricated event; integration test with real NATS
