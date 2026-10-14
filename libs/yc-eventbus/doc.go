// Package eventbus wraps NATS JetStream for yc-platform. It provides the
// transactional outbox, envelope marshaling, tenant-scoped subject routing,
// consumer-side idempotency, DLQ handling, and schema validation.
package eventbus