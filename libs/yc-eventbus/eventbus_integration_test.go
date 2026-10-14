//go:build integration

package eventbus_test

// Full publish/consume/DLQ tests require testcontainers for Postgres and
// NATS. These will be added when yc-testkit is implemented. The outline
// of coverage this file will provide:
//
//  1. Publish inserts to outbox within the caller's transaction; a
//     transaction rollback leaves no outbox row.
//  2. The relay publishes the outbox row to NATS; published_at is set.
//  3. A subscriber receives the event, invokes the handler, and the
//     handler's work plus processed_events insert are atomic.
//  4. A duplicate event_id is deduped per consumer (same consumer skips;
//     different consumer processes).
//  5. A handler returning ErrPoison routes the message to DLQ immediately.
//  6. A handler returning a retriable error retries up to MaxRetries,
//     then routes to DLQ with reason=exhausted.
//  7. A malformed envelope (invalid JSON) routes to DLQ with reason=poison.
//  8. Cleanup goroutine deletes old published outbox rows and
//     processed_events rows.
//  9. Outbox publishing with NATS down retries, succeeds on recovery.
//
// For now, run unit tests only.