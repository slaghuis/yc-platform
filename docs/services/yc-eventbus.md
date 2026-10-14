# yc-eventbus — Shared Library Specification

## Overview

`yc-eventbus` is a Go library used by every service in the monorepo for publishing and consuming events on NATS JetStream. It encapsulates envelope construction, tenant subject routing, the transactional outbox pattern, consumer-side idempotency, DLQ handling, and schema validation.

**No service touches `nats.go` directly.** All event interaction flows through this library.

## Location in Monorepo

```
/libs/yc-eventbus/
```

## Responsibilities

1. Construct and validate the standard event envelope (ADR-007)
2. Build tenant-scoped subjects (ADR-001, ADR-008)
3. Implement the transactional outbox pattern (ADR-006)
4. Publish events to NATS JetStream with retries and backoff
5. Consume events with automatic idempotency deduplication
6. Route unprocessable messages to tenant DLQ subjects (ADR-003)
7. Validate payloads against JSON Schema from `yc-event-schemas`
8. Provide a CLI tool for manual DLQ replay
9. Enrich spans with correlation and causation IDs for OpenTelemetry

## Public API (Illustrative)

```go
package eventbus

type Config struct {
    NATSUrl           string
    ServiceName       string
    SchemaRepoPath    string
    OutboxDBHandle    *sql.DB
    OutboxPollMS      int        // default 100
    MaxPublishRetries int        // default 5
    Logger            Logger
    Tracer            trace.Tracer
}

type Client struct { ... }

func NewClient(ctx context.Context, cfg Config) (*Client, error)

// Publishing (within a service DB transaction)
func (c *Client) Publish(ctx context.Context, tx *sql.Tx, evt Event) error

type Event struct {
    EventType     string      // e.g., "order.placed.v1"
    TenantID      string      // "_platform" for platform events
    Actor         Actor
    CorrelationID string      // propagated or generated
    CausationID   string      // event_id of parent, if any
    Payload       any         // marshalled to JSON, validated against schema
}

// Consuming
type Handler func(ctx context.Context, evt ConsumedEvent) error

type ConsumedEvent struct {
    Envelope Envelope
    Payload  json.RawMessage
}

type SubscribeOpts struct {
    Subject       string         // e.g., "yc.*.order.placed.v1"
    DurableName   string         // consumer group name
    MaxRetries    int            // default 5
    BackoffPolicy BackoffPolicy  // exponential by default
    Concurrency   int            // default 1
}

func (c *Client) Subscribe(ctx context.Context, opts SubscribeOpts, h Handler) error

// Error signalling from handlers
var ErrPoison = errors.New("eventbus: poison message, send to DLQ")
var ErrRetriable = errors.New("eventbus: retriable error")

// DLQ replay CLI
// Usage: yc-eventbus-dlq replay --subject "yc.<tenant>.<domain>.dlq.v1" --event-id "..."
```

## Envelope Format

As per ADR-007 and the event taxonomy. Enforced by this library — services construct `Event`, the library wraps it in the full envelope before publishing.

## Outbox Pattern Implementation

### Required Table in Each Service's Database

```sql
CREATE TABLE outbox (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     TEXT NOT NULL,
    subject       TEXT NOT NULL,
    envelope      JSONB NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    published_at  TIMESTAMPTZ,
    attempts      INT NOT NULL DEFAULT 0,
    last_error    TEXT,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_outbox_unpublished ON outbox (next_attempt_at) 
    WHERE published_at IS NULL;
```

### Relay Behavior

- Spawns a background goroutine on `NewClient`
- Polls `WHERE published_at IS NULL AND next_attempt_at <= NOW()` every `OutboxPollMS` (default 100ms)
- Publishes in batches (default 50)
- On success, sets `published_at`
- On failure, increments `attempts`, sets `last_error`, computes `next_attempt_at` via exponential backoff
- After `MaxPublishRetries`, publishes the event to the service's own local DLQ subject: `yc.<tenant_id>.<service_domain>.dlq.v1` with `dlq_metadata`

### Cleanup

- A separate goroutine deletes rows where `published_at < NOW() - INTERVAL '7 days'`
- Runs hourly

## Consumer-Side Idempotency

### Required Table in Each Service's Database

```sql
CREATE TABLE processed_events (
    event_id      UUID PRIMARY KEY,
    consumer      TEXT NOT NULL,
    processed_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_processed_events_time ON processed_events (processed_at);
```

- Library inserts `(event_id, consumer)` within the same transaction as the handler's work
- Duplicate `event_id` for same consumer → handler is skipped and ack is sent
- Cleanup: delete rows older than 30 days (hourly job)

## DLQ Handling

- Poison or exhausted-retry messages are republished to `yc.<tenant_id>.<domain>.dlq.v1`
- DLQ envelope wraps the original:
  ```json
  {
    "original_event": { ... full envelope ... },
    "dlq_metadata": {
      "consumer": "yc-orders",
      "attempts": 5,
      "first_failure": "...",
      "last_error": "...",
      "last_failure": "..."
    }
  }
  ```
- yc-monitor subscribes to `yc.*.*.dlq.v1` for alerting (ADR-003)

## Schema Validation

- On `Publish`: library loads the JSON Schema from `yc-event-schemas` matching the `event_type` and validates the payload
  - **Dev mode**: validation failure returns an error (fatal)
  - **Prod mode**: validation failure logs + emits metric + still publishes (configurable; default to fatal)
- On `Subscribe`: library checks that the envelope `tenant_id` equals the subject's tenant segment, then validates the envelope structure, then the payload against the schema
  - Tenant mismatch → `ErrPoison` → `_platform` DLQ (ADR-001)
  - Validation failure → `ErrPoison` → DLQ

## Observability

- Every publish/consume emits OpenTelemetry spans with:
  - `event.type`
  - `event.id`
  - `event.correlation_id`
  - `event.causation_id`
  - `event.tenant_id`
  - `event.subject`
- Correlation and causation IDs propagate via span attributes and context

## Database

This library does not own a database. It operates on the host service's database via the handle passed in `Config.OutboxDBHandle`.

## Configuration

Driven by the host service's config, passed into `Config` at construction.

## Dependencies

- `github.com/nats-io/nats.go` (JetStream client)
- `github.com/xeipuuv/gojsonschema` or `github.com/santhosh-tekuri/jsonschema/v5`
- `go.opentelemetry.io/otel`
- `github.com/jackc/pgx/v5` (database interactions)
- `github.com/golang-migrate/migrate/v4` (not directly; migrations are per-service, but the library provides the SQL to include)

## Published Events

Library does not publish on its own behalf — it publishes on behalf of host services. However, the DLQ wrapping is library-owned.

## Subscribed Events

None directly. The library provides subscription infrastructure to host services.

## Open Questions / Future Scope

- Dynamic schema reload (currently loaded at startup)
- Compression for large payloads
- Encryption at rest for sensitive event payloads (currently relies on JetStream file encryption)
- Consumer rate limiting per tenant
