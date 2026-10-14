package eventbus

import "context"

// Validator validates an event envelope and payload against a schema. The
// library calls Validator before publishing (if configured) and before
// handing a message to a consumer handler.
//
// Returning a nil error allows the event through. Returning any error
// treats the event as invalid; publish fails, and consume routes to DLQ
// as a poison message.
type Validator interface {
	// ValidateEnvelope checks envelope structure (not payload).
	ValidateEnvelope(ctx context.Context, env *Envelope) error

	// ValidatePayload checks the payload against the schema identified
	// by env.EventType.
	ValidatePayload(ctx context.Context, env *Envelope) error
}

// NoopValidator accepts every event. Suitable for tests and for services
// still in early development; real deployments must wire a real validator.
type NoopValidator struct{}

func (NoopValidator) ValidateEnvelope(context.Context, *Envelope) error { return nil }
func (NoopValidator) ValidatePayload(context.Context, *Envelope) error  { return nil }