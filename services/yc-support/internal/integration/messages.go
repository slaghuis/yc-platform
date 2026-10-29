// Package integration calls other services' internal APIs through
// yc-gateway (ADR-021): yc-message to notify customers and the shop, and
// yc-tenant for the shop's contact email.
package integration

import (
	"context"
	"net/http"

	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
)

// Caller makes internal calls.
type Caller interface {
	Call(ctx context.Context, method, path string, opts httpkit.CallOptions, in, out any) error
}

// Messages is yc-message's send API.
type Messages struct {
	client Caller
}

// NewMessages returns the client.
func NewMessages(client Caller) *Messages {
	return &Messages{client: client}
}

// Send is one message for POST /internal/send. Address the recipient by
// UserID (yc-message picks their preferred channel, ADR-023) or by Channel
// and Address.
type Send struct {
	// Type names the template, e.g. support_ack.
	Type string
	// Reference makes retries send the message once.
	Reference string
	UserID    string
	Channel   string
	Address   string
	Context   map[string]any
}

type sendRequest struct {
	Type      string         `json:"type"`
	Reference string         `json:"reference"`
	UserID    string         `json:"user_id,omitempty"`
	Channel   string         `json:"channel,omitempty"`
	Address   string         `json:"address,omitempty"`
	Context   map[string]any `json:"context"`
}

// Send queues a message with yc-message. Retries reuse the reference as
// idempotency key, so a message is sent once.
func (m *Messages) Send(ctx context.Context, s Send) error {
	if s.Context == nil {
		s.Context = map[string]any{}
	}
	return m.client.Call(ctx, http.MethodPost, "/internal/send",
		httpkit.CallOptions{IdempotencyKey: s.Reference, Attempts: 3},
		sendRequest(s), nil)
}
