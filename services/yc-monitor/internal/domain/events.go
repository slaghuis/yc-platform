// Package domain holds yc-monitor's types.
package domain

import (
	"encoding/json"
	"time"
)

// ReceivedEvent is a consumed platform event (Phase 1 stub).
type ReceivedEvent struct {
	EventID       string          `json:"event_id"`
	TenantID      string          `json:"tenant_id"`
	EventType     string          `json:"event_type"`
	SourceService string          `json:"source_service"`
	CorrelationID string          `json:"correlation_id,omitempty"`
	EventTime     time.Time       `json:"event_time"`
	Envelope      json.RawMessage `json:"envelope"`
	ReceivedAt    time.Time       `json:"received_at"`
}

// DLQEntry is a dead letter (ADR-003).
type DLQEntry struct {
	ID             string          `json:"id"`
	DLQSequence    uint64          `json:"dlq_sequence"`
	TenantID       string          `json:"tenant_id"`
	Service        string          `json:"service"`
	Consumer       string          `json:"consumer"`
	EventType      string          `json:"event_type"`
	EventID        string          `json:"event_id"`
	FailureReason  string          `json:"failure_reason"`
	Attempts       int             `json:"attempts"`
	LastError      string          `json:"last_error,omitempty"`
	FirstFailureAt time.Time       `json:"first_failure_at"`
	LastFailureAt  time.Time       `json:"last_failure_at"`
	Envelope       json.RawMessage `json:"envelope"`
	CreatedAt      time.Time       `json:"created_at"`
}

// Filter narrows a listing. An empty TenantID lists every tenant.
type Filter struct {
	TenantID string
	Limit    int
}
