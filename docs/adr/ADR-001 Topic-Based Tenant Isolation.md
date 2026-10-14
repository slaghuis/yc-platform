# ADR-001: Topic-Based Tenant Isolation

**Status:** Accepted
**Date:** 2025-01-15
**Context:** YC is a multi-tenant e-commerce platform. Events on the message bus must be isolated between tenants (shops) for security, auditability, and operational control.

## Decision

We will use **topic-based tenant isolation** on the message bus. The `tenant_id` is part of the subject hierarchy, not merely a field in the payload.

Subject pattern:
```
yc.<tenant_id>.<event_type>

event_type = <domain>[.<entity>].<action>.v<version>
```

The entity is **omitted when the event is about the domain's own aggregate**, and present otherwise:

| Event type | Domain | Entity | Action |
|---|---|---|---|
| `order.placed.v1` | order | — (the order itself) | placed |
| `inventory.stock.reserved.v1` | inventory | stock | reserved |
| `platform.tenant.provisioned.v1` | platform | tenant | provisioned |

An event type therefore has three or four dot-separated segments, and the entity never repeats the domain (`order.order.placed.v1` is invalid). `yc-eventbus` rejects any other shape before publishing.

*Amended 2026-10-14: the original text showed the entity as mandatory, which led to doubled names such as `order.order.placed.v1`. The optional-entity form is the intended convention.*

### Tenant routing guarantees

The tenant is always the **second** subject segment, whatever the shape of the event type. Dropping or adding an entity changes only the segments after the tenant, so `order.placed.v1` for shop `shop-abc` is always published on `yc.shop-abc.order.placed.v1` and is only delivered to consumers of that tenant's subjects (or authorized cross-tenant wildcards such as `yc.*.order.placed.v1`).

`yc-eventbus` enforces this:

1. **Tenant IDs occupy exactly one segment.** A tenant ID is `_platform` or a lowercase slug matching `^[a-z0-9]+(-[a-z0-9]+)*$`, at most 63 characters. Dots, wildcards (`*`, `>`), underscores, and uppercase are rejected, so an ID such as `shop.abc` can never publish on a subject that NATS reads as tenant `shop`, and no real tenant can collide with `_platform`. The publisher rejects an invalid ID before writing the outbox row.
2. **Envelope and subject must agree.** On every delivery the consumer compares the subject's tenant segment with `envelope.tenant_id`. A mismatch is poison: the event goes to the `_platform` DLQ and never reaches the handler or a tenant's DLQ, where a replay could deliver it to the wrong shop.
Handlers then act on `envelope.tenant_id`, which by rule 2 equals the subject tenant, and set it as `app.tenant_id` (ADR-017) so row-level security confines every read and write to that tenant.

The envelope schema (`envelope/v1.json`) applies the same tenant ID pattern.

*Amended 2026-10-14: tenant routing guarantees added.*

## Consequences

**Positive**
- Access policies on the bus can restrict producers and consumers to specific tenant subjects
- Per-tenant throttling, quotas, and quarantine are trivial
- Cross-tenant leakage requires an explicit security breach, not a code bug
- Admin tooling can wildcard across tenants (`yc.*.order.>`) when authorized

**Negative**
- Higher subject cardinality on the bus (manageable in NATS JetStream)
- Services must always know their `tenant_id` before publishing
- Replay/debugging requires per-tenant topic awareness

## Alternatives Considered

- **Payload-based isolation**: single topic per event type, filter by `tenant_id` in payload. Rejected — weaker security posture, easier to accidentally leak.

