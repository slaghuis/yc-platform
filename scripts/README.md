# JWT dev keys generated here by scripts/gen-dev-jwt-keys.sh
## Never commit *.pem files
`deploy/compose/dev-keys/.gitignore`
*.pem
`
---

## Schemas

### `schemas/yc-event-schemas/README.md`

```markdown
# yc-event-schemas

JSON Schemas for every event in the yc-platform event bus.

## Structure

- `envelope/v1.json` — common envelope wrapping every event
- `common/*.json` — reusable payload fragments (money, address, actor, etc.)
- `<domain>/<entity>.<action>.v<N>.json` — per-event schemas

See `docs/standards/event-conventions.md` for naming and versioning rules.

## Validation

```bash
task schema:validate
`

## Backward compatibility check (against main)
`
task schema:compat
`

## Generating Go types 

Changes here regenerate types in `libs/yc-event/generated/`:
`
task gen:events
`
CI fails if generated types are out of sync.
`
### `schemas/yc-event-schemas/envelope/v1.json`

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://yc/schemas/envelope/v1.json",
  "title": "Event Envelope v1",
  "description": "Standard envelope wrapping every event on the yc-platform bus.",
  "type": "object",
  "required": [
    "event_id",
    "event_type",
    "event_time",
    "tenant_id",
    "actor",
    "correlation_id",
    "source_service",
    "schema_version",
    "payload"
  ],
  "additionalProperties": false,
  "properties": {
    "event_id": {
      "type": "string",
      "format": "uuid",
      "description": "Unique identifier for this event; used for consumer-side idempotency."
    },
    "event_type": {
      "type": "string",
      "pattern": "^[a-z][a-z_]*\\.[a-z][a-z_]*\\.v[0-9]+$",
      "description": "Event type identifier matching the schema file name (minus domain prefix)."
    },
    "event_time": {
      "type": "string",
      "format": "date-time",
      "description": "UTC timestamp of event emission."
    },
    "tenant_id": {
      "type": "string",
      "minLength": 1,
      "description": "Tenant identifier. '_platform' for platform-level events."
    },
    "actor": {
      "$ref": "../common/actor.v1.json"
    },
    "correlation_id": {
      "type": "string",
      "format": "uuid",
      "description": "Shared across all events in a logical operation."
    },
    "causation_id": {
      "type": ["string", "null"],
      "format": "uuid",
      "description": "event_id of the direct parent event, if any."
    },
    "source_service": {
      "type": "string",
      "pattern": "^yc-[a-z]+$",
      "description": "Service that emitted this event."
    },
    "schema_version": {
      "type": "integer",
      "minimum": 1,
      "description": "Schema version matching the version in event_type."
    },
    "payload": {
      "type": "object",
      "description": "Event-specific data. Validated against the event's own schema."
    }
  }
}
`

