# ADR-004: Retention Policy by Domain

**Status:** Accepted
**Date:** 2025-01-15

## Decision

Each event stream has a declared retention policy, configured per JetStream stream.

| Domain | Retention | Rationale |
|---|---|---|
| `audit.*` | **Infinite** (archived to cold storage after 2 years) | Compliance, forensic |
| `order.*`, `payment.*`, `checkout.*` | **7 years** | Financial & tax compliance |
| `shipping.*` | **2 years** | Dispute resolution window |
| `identity.*` (except `deleted`) | **1 year** | Security investigation |
| `identity.user.deleted.v1` | **7 years** | Proof of GDPR compliance |
| `inventory.*` | **1 year** | Operational investigation |
| `product.*`, `pricing.*` | **1 year** | Reconstruct catalog state |
| `review.*` | **2 years** | Moderation disputes |
| `cart.*`, `preference.*` | **90 days** | Operational only |
| `message.*` | **1 year** | Delivery audit |
| `image.*` | **7 days** | Short-lived processing events |
| `monitor.*` | **30 days** | Operational telemetry |
| `platform.*` | **Infinite** | Tenant lifecycle must be fully traceable |
| `*.dlq.v1` | **90 days** | Human investigation window |

## Rules

1. Retention is enforced at the JetStream stream level, not application level
2. Services producing events do not care about retention — the bus owns it
3. Changes to retention policy require an ADR update
4. Long-retention streams (`audit`, `order`, `payment`) are configured with **file storage**, not memory
5. GDPR trumps retention: when a user is deleted, their events are **anonymized in place**, not deleted (see ADR-005)
