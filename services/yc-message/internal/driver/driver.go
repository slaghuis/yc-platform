// Package driver holds yc-message's channel drivers: SMTP email and a fake
// SMS sender for development.
package driver

import (
	"context"
	"errors"

	"github.com/slaghuis/yc-platform/services/yc-message/internal/domain"
)

// Outgoing is a rendered message ready to send.
type Outgoing struct {
	To      string
	Subject string
	Body    string
	Format  domain.Format
}

// Driver sends messages on one channel.
type Driver interface {
	Name() string
	Channel() domain.Channel
	// Send delivers the message and returns the provider's message ID.
	Send(ctx context.Context, m Outgoing) (string, error)
}

// ErrPermanent marks a failure that retrying will not fix, such as an
// address the provider refuses.
var ErrPermanent = errors.New("permanent delivery failure")
