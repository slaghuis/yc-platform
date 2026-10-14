package eventbus

import (
	"encoding/json"
	"time"
)

// Envelope is the standard wrapper for every event on the bus. Fields and
// semantics match envelope/v1.json in yc-event-schemas.
type Envelope struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	EventTime     time.Time       `json:"event_time"`
	TenantID      string          `json:"tenant_id"`
	Actor         Actor           `json:"actor"`
	CorrelationID string          `json:"correlation_id"`
	CausationID   string          `json:"causation_id,omitempty"`
	SourceService string          `json:"source_service"`
	SchemaVersion int             `json:"schema_version"`
	Payload       json.RawMessage `json:"payload"`
}

// Actor identifies who caused the event.
type Actor struct {
	Type           string `json:"type"`
	ID             string `json:"id"`
	AccessTokenJTI string `json:"access_token_jti,omitempty"`
}

// Event is the input to Publish. The library builds an Envelope around it.
type Event struct {
	// EventType must match a registered schema identifier
	// (e.g., "order.placed.v1").
	EventType string

	// TenantID identifies the owning tenant; use "_platform" for
	// platform-level events (see ADR-008).
	TenantID string

	// Actor identifies the human user or service that caused this event.
	Actor Actor

	// CorrelationID groups all events in a logical operation. If empty,
	// the library generates one; prefer propagating from the triggering
	// context.
	CorrelationID string

	// CausationID is the event_id of the direct parent event, or empty
	// for user-triggered events.
	CausationID string

	// Payload is marshalled to JSON. Must satisfy the schema for
	// EventType.
	Payload any
}

// DLQEnvelope wraps an original envelope with metadata about why it
// landed in the dead-letter queue.
type DLQEnvelope struct {
	OriginalEvent Envelope    `json:"original_event"`
	DLQMetadata   DLQMetadata `json:"dlq_metadata"`
}

// DLQMetadata records consumer, attempt count, and failure context for a
// dead-lettered event.
type DLQMetadata struct {
	Service       string    `json:"service,omitempty"` // the consuming service
	Consumer      string    `json:"consumer"`          // its durable consumer name
	Attempts      int       `json:"attempts"`
	FirstFailure  time.Time `json:"first_failure"`
	LastFailure   time.Time `json:"last_failure"`
	LastError     string    `json:"last_error"`
	FailureReason string    `json:"failure_reason"` // "exhausted" | "poison"
}
