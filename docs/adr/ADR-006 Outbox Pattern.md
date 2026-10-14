# ADR-006: Outbox Pattern

**Status:** Accepted
**Date:** 2025-01-15
**Context:** Services write state (Postgres) and publish events (NATS). Without coordination, a crash between the two causes either lost events or phantom events.

## Decision

Every service that both persists state and publishes events uses the **transactional outbox pattern**.

## Mechanism

1. Each service's database has an `outbox` table. The canonical definition is `libs/yc-eventbus/schema.sql`:
   ```sql
   CREATE TABLE outbox (
     id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
     tenant_id       TEXT NOT NULL,
     subject         TEXT NOT NULL,
     envelope        JSONB NOT NULL,   -- the full event envelope, not just the payload
     created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
     published_at    TIMESTAMPTZ,
     attempts        INT NOT NULL DEFAULT 0,
     last_error      TEXT,
     next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
   );
   CREATE INDEX idx_outbox_unpublished ON outbox (next_attempt_at) WHERE published_at IS NULL;
   ```
2. State changes and the corresponding outbox insert happen in the **same database transaction**
3. A **relay goroutine** inside each service polls the outbox for rows whose `next_attempt_at` has passed and publishes them to NATS JetStream
4. On successful publish, `published_at` is set
5. On failure, `attempts++`, `last_error` is recorded, and `next_attempt_at` moves forward by an exponential backoff
6. After N failures, the entry is published to the local DLQ subject with the service's identity

## Standardization

- The `yc-eventbus` Go library implements the outbox for every service
- The AI code factory scaffolds the `outbox` table migration and relay into every new service
- Services call `eventbus.Publish(tx, event)` — the library handles the rest

## Consequences

**Positive**
- Exactly-once from the service's authoring perspective (consumers must still be idempotent)
- Survives service crashes mid-publish
- Natural audit trail of outgoing events

**Negative**
- Added write per state change
- Relay adds latency (default poll: 100ms)
- Outbox table requires maintenance (vacuum/archival) — delete rows older than 7 days with `published_at IS NOT NULL`

## Consumer-Side Idempotency

Consumers **must dedupe on `event_id`**. The `yc-eventbus` library provides a Postgres-backed `processed_events` table and a wrapper to make this transparent.
