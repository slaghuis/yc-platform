# ADR-007: Schema Registry

**Status:** Accepted
**Date:** 2025-01-15
**Context:** With dozens of event types across many services, schema drift is inevitable without governance.

## Decision

Event schemas are **versioned, source-controlled JSON Schema documents** in a shared repository: `yc-event-schemas`.

## Structure

```
yc-event-schemas/
├── envelope/
│   └── v1.json
├── order/
│   ├── order.placed.v1.json
│   ├── order.placed.v2.json
│   └── order.shipped.v1.json
├── cart/
│   └── cart.item.added.v1.json
...
```

## Rules

1. **Every event in the taxonomy has a schema** — no exceptions
2. **Schemas are immutable** once released — changes require a new version (`.v2`)
3. **Backward-compatible additions** (new optional fields) do NOT require a version bump
4. **Breaking changes** (removal, rename, type change) DO require a new version — both v1 and v2 run in parallel until all consumers migrate
5. The `yc-eventbus` library **validates every outgoing event** against the schema before publishing (dev mode: fatal; prod mode: log + metric + publish)
6. CI in the schema repo validates: JSON Schema correctness, no breaking changes to existing version, naming convention
7. The AI code factory **reads from this repo** to generate Go structs for every service

## Future Evolution

If/when operational pain justifies it, we migrate to a runtime registry (Apicurio, Buf Schema Registry). For now, source control is the registry. ADR to be revisited when:
- Event type count exceeds ~100, OR
- Multiple teams produce events, OR
- External consumers (partner integrations) exist
