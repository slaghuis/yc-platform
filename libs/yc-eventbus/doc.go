// Package eventbus is the yc-platform event bus client. It wraps NATS
// JetStream with:
//
//   - The transactional outbox pattern. Services publish by inserting
//     into their own outbox table within the state-change transaction;
//     a background relay forwards to NATS.
//   - Tenant-scoped subject construction and consumer-group routing.
//   - Consumer-side idempotency via a processed_events table.
//   - Dead-letter handling. Poison or exhausted messages land in
//     yc.<tenant>.<domain>.dlq.v1 with the original envelope preserved.
//   - Schema validation hook — callers inject a Validator; the library
//     itself has no schema dependency.
//   - OpenTelemetry tracing on publish and consume.
//
// Services construct a Client at startup, keep it alive for the process
// lifetime, and call Publish/Subscribe. Example:
//
//	client, err := eventbus.NewClient(ctx, eventbus.Config{
//	    NATSURL:      "nats://localhost:4222",
//	    ServiceName:  "yc-identity",
//	    DB:           dbPool,
//	    Validator:    schemaValidator, // or eventbus.NoopValidator{}
//	})
//
//	// Publish within a service transaction:
//	tx, _ := db.Begin(ctx)
//	defer tx.Rollback(ctx)
//	_, _ = tx.Exec(ctx, "UPDATE users SET ... WHERE id=$1", id)
//	_ = client.Publish(ctx, tx, eventbus.Event{
//	    EventType: "identity.user.registered.v1",
//	    TenantID:  tenantID,
//	    Actor:     actor,
//	    Payload:   payload,
//	})
//	_ = tx.Commit(ctx)
//
//	// Subscribe:
//	err = client.Subscribe(ctx, eventbus.SubscribeOpts{
//	    Subject:     "yc.*.identity.user.registered.v1",
//	    DurableName: "yc-message-identity-user-registered",
//	}, handleUserRegistered)
//
// See docs/standards/event-conventions.md for naming and payload rules.
package eventbus
