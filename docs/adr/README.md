# Architecture Decision Records

| # | Title | Status |
|---|---|---|
| 001 | Topic-Based Tenant Isolation | Accepted |
| 002 | Message Bus — NATS JetStream | Accepted |
| 003 | Dead Letter Topics | Accepted |
| 004 | Retention Policy by Domain | Accepted |
| 005 | GDPR Cascade | Accepted |
| 006 | Outbox Pattern | Accepted |
| 007 | Schema Registry | Accepted |
| 008 | Platform Tenant Convention | Accepted |
| 009 | No Storage of Payment Card Data | Accepted |
| 010 | Guest Checkout Requirements | Accepted |
| 011 | Price Quote Validity | Accepted |
| 012 | Inventory Management Modes | Accepted |
| 013 | Payment Gateways — Stripe and Yoco | Accepted |
| 014 | Single Store Currency, Display Currency Conversion | Accepted |
| 015 | Address Storage as JSON | Accepted |
| 016 | Tax within Pricing — Multiple Tax Rates per Product | Accepted |
| 017 | Database Per Service | Accepted |

Each ADR is in its own file: `001-topic-based-tenant-isolation.md`, etc.
These were produced in earlier design sessions; populate each file from
those sessions.

## Template

New ADRs follow this template:

```markdown
# ADR-NNN: <Title>

**Status:** Proposed | Accepted | Superseded by ADR-XXX
**Date:** YYYY-MM-DD
**Context:** <What's the problem and what's forcing the decision?>

## Decision

<The decision itself. Specific. Concrete.>

## Rules

<Numbered constraints that follow from the decision.>

## Consequences

**Positive**
- ...

**Negative**
- ...

## Alternatives Considered

- **<Alternative>**: <Why rejected>