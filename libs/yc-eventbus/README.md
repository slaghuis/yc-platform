# yc-eventbus

The yc-platform event bus client. NATS JetStream with transactional outbox,
consumer-side idempotency, DLQ handling, and schema validation hook.

## What it does

- **Transactional outbox**. `Publish(ctx, tx, event)` inserts into your
  service's outbox within your transaction. A relay goroutine publishes to
  NATS; a failed publish retries with backoff and ends in DLQ.
- **Tenant-scoped subjects**. `yc.<tenant>.<domain>[.<entity>].<action>.v<N>` (entity omitted for the domain's own aggregate, per ADR-001)
  constructed from envelope fields; services never build subjects by hand.
- **Durable consumers**. `Subscribe(ctx, opts, handler)` creates a
  JetStream durable consumer per `(subject, DurableName)`.
- **Idempotency**. The `processed_events` table dedupes by
  `(event_id, consumer)`.
- **Dead-letter queue**. Poison messages or exhausted retries land in
  `yc.<tenant>.<domain>.dlq.v1` with the original envelope preserved.
- **Schema validation**. Pluggable `Validator` interface; `NoopValidator`
  for early development.
- **OpenTelemetry**. Every publish and consume creates a span; standard
  metrics emitted.

## Usage

See `doc.go` for a complete example. In short:

```go
client, err := eventbus.NewClient(ctx, eventbus.Config{
    NATSURL:     os.Getenv("NATS_URL"),
    ServiceName: "yc-identity",
    DB:          db, // *pgxpool.Pool
    Validator:   validator,
    Streams: []eventbus.StreamConfig{
        {
            Name:     "YC_IDENTITY",
            Subjects: []string{"yc.*.identity.>"},
            Storage:  jetstream.FileStorage,
            MaxAge:   24 * time.Hour * 365,
        },
    },
})

// Publish within a transaction.
err = client.Publish(ctx, tx, eventbus.Event{
    EventType: "identity.user.registered.v1",
    TenantID:  tenantID,
    Actor:     eventbus.Actor{Type: "customer", ID: userID},
    Payload:   payload,
})

// Subscribe.
err = client.Subscribe(ctx, eventbus.SubscribeOpts{
    Subject:     "yc.*.identity.user.registered.v1",
    DurableName: "yc-message-welcome",
    MaxRetries:  5,
}, handleUserRegistered)
```

## Error contract for handlers


| Handler return | Outcome |
| -------------- | ------- |
| nil | Ack. Message marked in processed_events. |
| fmt.Errorf("%w: ...", ErrPoison) | Immediately routed to DLQ with reason=poison. |
| Any other non-nil error | NAK with backoff; after MaxRetries, DLQ with reason=exhausted.|

## Required database schema
Services' first migration (`001_eventbus_tables.up.sql`) creates the two tables this library needs: `outbox` and `processed_events`. The canonical schema is in `schema.sql` in this directory.
## Configuration reference
See `config.go`. Noteworthy defaults:
| Setting | Default |
| ------- | ------- |
| OutboxPollInterval | 100 ms |
| OutboxBatchSize | 50 |
| OutboxMaxAttempts | 5 |
| OutboxBackoff | 1s, 5s, 30s, 2m, 10m |
| ProcessedEventsRetention | 30 days |
| OutboxCleanupRetention | 7 days |
| CleanupInterval | 1 hour |

## Testing
For unit tests of code that uses the client, inject a Client or wrap it in your own interface. For integration tests, use `NewTestClient` from `testing.go`:

```go
func TestWithEventBus(t *testing.T) {
    ctx := context.Background()
    db := testkit.StartPostgres(t)
    natsURL := testkit.StartNATS(t)

    client := eventbus.NewTestClient(t, ctx, natsURL, db,
        eventbus.DefaultTestStream("YC_TEST"))

    // ... publish and subscribe ...
}
```
The `yc-testkit` library (not yet implemented) will provide `StartPostgres` and `StartNATS` via testcontainers.
## Operational notes
- **Streams are created at startup**. If a service declares a stream in Config.Streams, NewClient calls CreateOrUpdateStream. For production, stream creation and lifecycle management should be owned by ops tooling, with Config.Streams empty in services.
- **DLQ monitoring**. yc-monitor subscribes to yc.*.*.dlq.v1 across all tenants. Replay via yc-dlq-cli replay.
- **Metrics emitted**: event.publish.total, event.consume.total, event.publish_duration_seconds, event.processing_duration_seconds. See metrics.go.
## See also

- `docs/standards/event-conventions.md` — naming and payload rules
- `docs/adr/ADR-001 Topic-Based Tenant Isolation.md`
- `docs/adr/ADR-003 Dead Letter Topics.md`
- `docs/adr/ADR-006 Outbox Pattern.md`
- `docs/adr/ADR-007 Schema Registry.md`

## Coverage of the Phase 1 requirements

Checking against the `yc-eventbus` requirements in Phase 1 of the implementation plan:

| Requirement | Covered |
|---|---|
| NATS JetStream publishing | ✓ |
| Transactional outbox pattern | ✓ |
| Tenant-scoped subject construction | ✓ (`SubjectForEvent`) |
| Envelope marshalling with full fields | ✓ |
| Consumer-side idempotency via `processed_events` | ✓ |
| DLQ routing for poison messages | ✓ |
| DLQ routing for exhausted retries | ✓ |
| DLQ envelope preserves original + metadata | ✓ |
| Schema validation hook (pluggable Validator) | ✓ |
| OpenTelemetry tracing (publish + consume) | ✓ |
| Standard metrics (counters, histograms) | ✓ |
| Durable consumer configuration | ✓ |
| Exponential backoff on retry | ✓ |
| Cleanup goroutine for old rows | ✓ |
| Graceful shutdown | ✓ |

## What's deliberately out of scope for v1

1. **A real JSON Schema validator**. We ship with `NoopValidator` and the interface. The real validator depends on `yc-event-schemas` being populated and the `yc-schema-gen` tool being functional. Both are in Phase 1 but behind this library in the dependency order — services will develop against `NoopValidator` until the validator lands.

2. **Observable gauge for outbox pending count**. The hook is in `cleanup.go` but doesn't currently emit a metric. OpenTelemetry's observable gauge requires registering a callback, which is a small refactor to the metrics init path. Added when operational dashboards actually need it.

3. **Parallel consumer workers per subscription**. `Concurrency` is accepted but currently only sizes the pull batch; actual parallelism is one goroutine per subscription. If a slow handler becomes a bottleneck, split it into N workers reading from a channel. Keeping it single-worker for v1 avoids ordering surprises.

4. **A full `yc-dlq-cli` tool**. The library produces the DLQ messages correctly; replay tooling is a separate deliverable listed as a Phase 1 tool.

5. **Multi-instance outbox relay**. The SQL uses `FOR UPDATE SKIP LOCKED`, which already supports multiple relay processes competing safely. We don't currently run more than one per service; the pattern is future-proof.

6. **Automatic span-based correlation ID propagation on consume**. The consumer uses `c.bgCtx` as the parent, which means consumed events don't inherit the trace from the producer. To do that properly, we'd marshal the W3C trace context into the envelope and reconstruct it on consume. The `correlation_id` and `causation_id` fields in the envelope achieve the correlation-tracking goal; full trace propagation can be a Phase 1 refinement when we see how traces actually look in Jaeger.

## Known rough edges to clean up

1. **`versionFromEventType` reinvents parsing** instead of using `strconv.Atoi` on a cleanly split segment. The hand-rolled parsing was premature optimization. Should be:
   ```go
   segments := strings.Split(eventType, ".")
   return strconv.Atoi(strings.TrimPrefix(segments[len(segments)-1], "v"))
   ```
2. `metrics.go` uses `any` **for the stored instruments** and type-asserts in accessors. This is forced by the circular initialization problem — we can't type the fields with `metric.Int64Counter` because the init happens inside the client constructor. A cleaner alternative is a dedicated `instruments` struct built in initMetrics with properly typed fields. Minor refactor.
3. **The subscription goroutine** uses `c.bgCtx` as the parent context for every handler invocation. This means per-message cancellation can only come from client shutdown. For long-running handlers with per-message timeouts, we'd need to derive a child context with a deadline. Not needed for v1 but worth noting.
4. **No circuit breaker** around NATS publish failures. If NATS is down, the relay will keep hammering it at the configured interval. The retries-with-backoff on individual rows limit the blast radius, but a connection-level breaker would be a kinder operational posture. Postponed to Phase 9 hardening.
5. **`outbox.pending` is registered but never recorded.** It is meant to be the current count of unpublished outbox rows, which an up/down counter cannot track reliably (a rolled-back `Publish` transaction would leave it high). It should become an observable gauge whose callback counts unpublished rows.
6. **Integration tests are only sketched**. The full integration test file is deliberately empty pending `yc-testkit`. Builds and unit tests pass; publish/consume behavior under real conditions is unverified at this moment.