# ADR-008: Platform Tenant Convention

**Status:** Accepted
**Date:** 2025-01-15
**Context:** Some events are not tenant-scoped: tenant provisioning, service registration, platform-wide monitoring. We need a convention.

## Decision

Introduce a reserved pseudo-tenant: **`_platform`**.

Subject pattern remains uniform:
```
yc._platform.<domain>.<entity>.<action>.v<version>
```

Examples:
- `yc._platform.tenant.provisioned.v1`
- `yc._platform.tenant.suspended.v1`
- `yc._platform.service.registered.v1`
- `yc._platform.gdpr.cascade_completed.v1`

## Rules

1. The `_platform` tenant is **reserved** — no real tenant may be named `_platform` (underscore prefix is forbidden for real tenants)
2. Only **system administrators** and **platform services** may publish to `yc._platform.>`
3. Only **system administrators** and **authorized platform services** may subscribe to `yc._platform.>`
4. Platform events **still carry `tenant_id: "_platform"`** in the envelope — the envelope stays uniform
5. Shop administrators have no visibility into `_platform` events
6. Retention for `_platform` events: **infinite** (see ADR-004)

## Rationale

- Keeps the subject pattern uniform across all events — tooling, libraries, and ACLs have one rule to apply
- Reserves a clear namespace for cross-cutting concerns
- Underscore prefix is a widely understood convention for "system-reserved"
- Avoids a special-case code path for "no tenant"

## Alternatives Considered

- **Separate subject root** (`yc-platform.<domain>...`): rejected, breaks uniformity
- **Omit tenant segment entirely** (`yc.<domain>...` when platform): rejected, requires every tool to understand the exception
