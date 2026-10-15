// Package events holds yc-audit's event subscribers: one durable consumer
// per audited event type.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	ycevents "github.com/slaghuis/yc-platform/libs/yc-events"
	"github.com/slaghuis/yc-platform/services/yc-audit/internal/domain"
)

// Handlers are the use cases the subscribers drive.
type Handlers interface {
	Record(ctx context.Context, env eventbus.Envelope) error
	UserDeleted(ctx context.Context, env eventbus.Envelope, p ycevents.IdentityUserDeletedV1) error
	CascadeCompleted(ctx context.Context, env eventbus.Envelope, p ycevents.PlatformGdprCascadeCompletedV1) error
}

// Subscription pairs a durable consumer with its handler.
type Subscription struct {
	Opts    eventbus.SubscribeOpts
	Handler eventbus.Handler
}

// Subscriptions are yc-audit's consumers: the GDPR events drive the
// register; every other audited event is recorded as it is.
func Subscriptions(h Handlers) []Subscription {
	out := make([]Subscription, 0, len(domain.Rules))
	for _, typ := range domain.AuditedTypes() {
		var handle eventbus.Handler
		switch typ {
		case ycevents.TypeIdentityUserDeletedV1:
			handle = typed(h.UserDeleted)
		case ycevents.TypePlatformGdprCascadeCompletedV1:
			handle = typed(h.CascadeCompleted)
		default:
			handle = func(ctx context.Context, evt eventbus.ConsumedEvent) error {
				return poisonInvalid(h.Record(ctx, evt.Envelope))
			}
		}
		out = append(out, Subscription{
			Opts:    eventbus.SubscribeOpts{Subject: "yc.*." + typ, DurableName: "yc-audit-" + durable(typ)},
			Handler: handle,
		})
	}
	return out
}

func typed[T any](fn func(ctx context.Context, env eventbus.Envelope, p T) error) eventbus.Handler {
	return func(ctx context.Context, evt eventbus.ConsumedEvent) error {
		var p T
		if err := json.Unmarshal(evt.Payload, &p); err != nil {
			return fmt.Errorf("%w: decode payload: %w", eventbus.ErrPoison, err)
		}
		return poisonInvalid(fn(ctx, evt.Envelope, p))
	}
}

// durable names a consumer after its event type: identity.user.deleted.v1
// becomes identity-user-deleted.
func durable(eventType string) string {
	return strings.NewReplacer(".", "-", "_", "-").Replace(strings.TrimSuffix(eventType, ".v1"))
}

// poisonInvalid sends events with invalid content to the DLQ rather than
// retrying them.
func poisonInvalid(err error) error {
	if ycerrors.HasCode(err, ycerrors.CodeValidationFailed) {
		return fmt.Errorf("%w: %w", eventbus.ErrPoison, err)
	}
	return err
}
