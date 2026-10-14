# yc-event-schemas — Repository Structure

## Overview

`yc-event-schemas` is a directory within the monorepo containing JSON Schema definitions for every event in the platform. It is the single source of truth for event payload structure (ADR-007).

## Location in Monorepo

```
/schemas/yc-event-schemas/
```

## Directory Structure

```
/schemas/yc-event-schemas/
├── envelope/
│   └── v1.json
├── platform/
│   ├── tenant.provisioned.v1.json
│   ├── tenant.suspended.v1.json
│   ├── tenant.deleted.v1.json
│   ├── tenant.configuration_changed.v1.json
│   ├── service.registered.v1.json
│   ├── service.deregistered.v1.json
│   └── gdpr.cascade_completed.v1.json
├── identity/
│   ├── visitor.created.v1.json
│   ├── user.registered.v1.json
│   ├── user.promoted.v1.json
│   ├── user.logged_in.v1.json
│   ├── user.password_reset_requested.v1.json
│   ├── user.deleted.v1.json
│   └── oauth_linked.v1.json
├── product/ ...
├── pricing/ ...
├── inventory/ ...
├── cart/ ...
├── preference/ ...
├── review/ ...
├── checkout/ ...
├── payment/ ...
├── orders/ ...
├── shipping/ ...
├── message/ ...
├── image/ ...
├── audit/ ...
├── monitor/ ...
├── common/
│   ├── money.v1.json
│   ├── address.v1.json
│   ├── line_item.v1.json
│   ├── guest_contact.v1.json
│   └── actor.v1.json
├── scripts/
│   ├── validate_all.sh
│   ├── check_backward_compat.sh
│   └── generate_go_types.sh
└── README.md
```

## Envelope Schema (abbreviated)

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://yc/schemas/envelope/v1.json",
  "type": "object",
  "required": [
    "event_id","event_type","event_time","tenant_id",
    "actor","correlation_id","source_service","schema_version","payload"
  ],
  "properties": {
    "event_id":        { "type": "string", "format": "uuid" },
    "event_type":      { "type": "string", "pattern": "^[a-z_]+\\.[a-z_]+\\.v[0-9]+$" },
    "event_time":      { "type": "string", "format": "date-time" },
    "tenant_id":       { "type": "string", "minLength": 1 },
    "actor":           { "$ref": "../common/actor.v1.json" },
    "correlation_id":  { "type": "string", "format": "uuid" },
    "causation_id":    { "type": ["string", "null"], "format": "uuid" },
    "source_service":  { "type": "string" },
    "schema_version":  { "type": "integer", "minimum": 1 },
    "payload":         { "type": "object" }
  }
}
```

## Rules

1. **One schema per event version**. Files named after the full event type, `<domain>/<event_type>.json` (e.g. `order/order.placed.v1.json`).
2. **Immutable once released**. Any breaking change creates a new `.v2.json`.
3. **Backward-compatible additions** (new optional fields) do NOT require a new version.
4. **Common fragments** live under `common/` and are referenced via `$ref`.
5. **CI validates**:
   - All schemas are valid JSON Schema
   - No existing versioned schema has had its structure altered in a breaking way
   - All referenced `$ref`s resolve
   - All event types mentioned in the taxonomy have a schema
6. **Go type generation**: `scripts/generate_go_types.sh` produces Go structs under `/libs/yc-eventbus/generated/` for every schema. Services import these types.

## Change Workflow

1. New event or new version → PR adding/updating the schema file
2. CI runs compatibility checks
3. Reviewer validates alignment with the taxonomy
4. Merge → generated Go types are regenerated and committed
5. Services update to the new types as needed

## Dependencies

Pure JSON Schema. No runtime code.

## Open Questions / Future Scope

- Migrate to a runtime registry (Apicurio, Buf) when scale demands
- Protobuf schemas instead of or alongside JSON Schema (if performance or binary compactness becomes a concern)
