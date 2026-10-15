// Package integration calls other services' internal APIs through
// yc-gateway (ADR-021): yc-message for channel verification codes.
package integration

import (
	"context"
	"net/http"
	"time"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
)

// Caller makes internal calls.
type Caller interface {
	Call(ctx context.Context, method, path string, opts httpkit.CallOptions, in, out any) error
}

// Messages is yc-message's verification API.
type Messages struct {
	client Caller
}

// NewMessages returns the client.
func NewMessages(client Caller) *Messages {
	return &Messages{client: client}
}

// Verification is a code yc-message sent.
type Verification struct {
	VerificationID string     `json:"verification_id"`
	ExpiresAt      time.Time  `json:"expires_at"`
	ConfirmedAt    *time.Time `json:"confirmed_at"`
}

type channelAddress struct {
	UserID  string `json:"user_id"`
	Channel string `json:"channel"`
	Address string `json:"address"`
	Code    string `json:"code,omitempty"`
}

// StartVerification sends a new code to a user's channel address.
func (m *Messages) StartVerification(ctx context.Context, userID, channel, address string) (Verification, error) {
	var out Verification
	err := m.client.Call(ctx, http.MethodPost, "/internal/channels/verify-start", httpkit.CallOptions{Attempts: 1},
		channelAddress{UserID: userID, Channel: channel, Address: address}, &out)
	return out, relay(err)
}

// ConfirmVerification checks a code yc-message sent to the address.
func (m *Messages) ConfirmVerification(ctx context.Context, userID, channel, address, code string) (Verification, error) {
	var out Verification
	err := m.client.Call(ctx, http.MethodPost, "/internal/channels/verify-confirm", httpkit.CallOptions{Attempts: 1},
		channelAddress{UserID: userID, Channel: channel, Address: address, Code: code}, &out)
	return out, relay(err)
}

// relay passes yc-message's answer to a bad request, such as a wrong code,
// on to the customer unchanged.
func relay(err error) error {
	if p, ok := httpkit.UpstreamProblem(err); ok && p.Status >= 400 && p.Status < 500 {
		return &ycerrors.Error{Code: p.Code, Status: p.Status, Message: p.Detail, Fields: p.Errors, Cause: err}
	}
	return err
}
