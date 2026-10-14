# ADR-001: Topic-Based Tenant Isolation

**Status:** Accepted
**Date:** 2025-01-15
**Context:** YC is a multi-tenant e-commerce platform. Events on the message bus must be isolated between tenants (shops) for security, auditability, and operational control.

## Decision

We will use **topic-based tenant isolation** on the message bus. The `tenant_id` is part of the subject hierarchy, not merely a field in the payload.

Subject pattern:
```
yc.<tenant_id>.<domain>.<entity>.<action>.v<version>
```

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

