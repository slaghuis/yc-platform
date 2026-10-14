# yc-event-schemas

JSON Schemas for every event in the yc-platform event bus (ADR-007).

## Structure

- `envelope/v1.json` — common envelope wrapping every event
- `common/*.json` — reusable payload fragments (money, address, actor, etc.)
- `<domain>/<event_type>.json` — per-event schemas, named after the full event type (e.g. `order/order.placed.v1.json`)

See `docs/standards/event-conventions.md` for naming and versioning rules, and `docs/onboarding/adding-an-event.md` to add one.

## Validation

```bash
task schema:validate
```

## Backward compatibility check (against main)

```bash
task schema:compat
```

## Generating Go types

Changes here regenerate types in `libs/yc-events/generated/`:

```bash
task gen:events
```

CI fails if generated types are out of sync.
