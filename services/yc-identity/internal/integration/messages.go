// Package integration calls other services' internal APIs through
// yc-gateway (ADR-021): yc-message for password reset links.
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

type sendRequest struct {
	Type      string         `json:"type"`
	Reference string         `json:"reference"`
	UserID    string         `json:"user_id"`
	Channel   string         `json:"channel"`
	Address   string         `json:"address"`
	Context   map[string]any `json:"context"`
}

// SendPasswordReset emails a reset link. reference makes retries send it
// once.
func (m *Messages) SendPasswordReset(ctx context.Context, reference, userID, email, resetURL, expiresAt string) error {
	return m.client.Call(ctx, http.MethodPost, "/internal/send",
		httpkit.CallOptions{IdempotencyKey: reference, Attempts: 3},
		sendRequest{
			Type:      "password_reset",
			Reference: reference,
			UserID:    userID,
			Channel:   "email",
			Address:   email,
			Context:   map[string]any{"reset_url": resetURL, "expires_at": expiresAt},
		}, nil)
}
