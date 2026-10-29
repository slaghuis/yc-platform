// Package events holds yc-support's event subscribers.
package events

import (
	"context"
	"fmt"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	ycevents "github.com/slaghuis/yc-platform/libs/yc-events"
)

// Handlers are the use cases the subscribers drive.
type Handlers interface {
	ForgetUser(ctx context.Context, tenantID string, e ycevents.IdentityUserDeletedV1) error
}

// Subscription pairs a durable consumer with its handler.
type Subscription struct {
	Opts    eventbus.SubscribeOpts
	Handler eventbus.Handler
}

// Subscriptions are yc-support's consumers: GDPR erasure when identity
// deletes a user (ADR-005). Claiming a visitor's requests is not driven by
// identity.user.registered.v1, whose email is not verified; yc-identity
// calls the claim API once it is.
func Subscriptions(h Handlers) []Subscription {
	gdprOpts, gdpr := ycevents.OnUserDeleted("yc-support-gdpr", func(ctx context.Context, tenantID string, e ycevents.IdentityUserDeletedV1) error {
		return poisonInvalid(h.ForgetUser(ctx, tenantID, e))
	})
	return []Subscription{{Opts: gdprOpts, Handler: gdpr}}
}

// poisonInvalid sends events with invalid content to the DLQ rather than
// retrying them.
func poisonInvalid(err error) error {
	if ycerrors.HasCode(err, ycerrors.CodeValidationFailed) {
		return fmt.Errorf("%w: %w", eventbus.ErrPoison, err)
	}
	return err
}
