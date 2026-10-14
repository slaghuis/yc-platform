# ADR-002: Message Bus — NATS JetStream

**Status:** Accepted
**Date:** 2025-01-15
**Context:** We need an event bus supporting hierarchical subjects, durable streams, replay, and good Go ergonomics.

## Decision

Use **NATS JetStream** as the platform event bus.

## Consequences

**Positive**
- Native hierarchical subjects align perfectly with our tenant isolation strategy
- Excellent Go SDK (`nats.go`)
- Lightweight ops footprint; can run embedded in dev
- Built-in persistence, replay, consumer groups, and DLQ support
- Subject-based ACLs for tenant isolation

**Negative**
- Team unfamiliar with NATS — learning curve
- Smaller ecosystem than Kafka
- Fewer managed service options than Kafka

## Mitigation

- Create a shared `yc-eventbus` Go library that wraps `nats.go` with:
  - Outbox pattern integration
  - Envelope marshalling
  - Tenant subject construction
  - Standard DLQ handling
- The AI code factory produces all publishers/subscribers through this library — services never touch `nats.go` directly.
