package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
)

// GDPR cascade actions (platform.gdpr.cascade_completed.v1, ADR-005).
const (
	GDPRDeleted       = "deleted"
	GDPRAnonymized    = "anonymized"
	GDPRPseudonymized = "pseudonymized"
)

// GDPRCompleted is the platform.gdpr.cascade_completed.v1 a service
// publishes, in the deleted user's tenant, once it has handled deleted:
// action is what it did and records how many records it changed.
func GDPRCompleted(tenantID, service string, deleted IdentityUserDeletedV1, action string, records int64, at time.Time) eventbus.Event {
	return eventbus.Event{
		EventType: TypePlatformGdprCascadeCompletedV1,
		TenantID:  tenantID,
		Actor:     eventbus.Actor{Type: "service", ID: service},
		Payload: PlatformGdprCascadeCompletedV1{
			UserID:      deleted.UserID,
			Pseudonym:   deleted.Pseudonym,
			Service:     service,
			Action:      action,
			RecordCount: records,
			CompletedAt: at.UTC(),
		},
	}
}

// OnUserDeleted subscribes durable to identity.user.deleted.v1 in every
// tenant. A payload that does not decode goes to the DLQ; fn's errors are
// retried, so fn must be idempotent.
func OnUserDeleted(durable string, fn func(ctx context.Context, tenantID string, e IdentityUserDeletedV1) error) (eventbus.SubscribeOpts, eventbus.Handler) {
	opts := eventbus.SubscribeOpts{Subject: "yc.*." + TypeIdentityUserDeletedV1, DurableName: durable}
	return opts, func(ctx context.Context, evt eventbus.ConsumedEvent) error {
		var e IdentityUserDeletedV1
		if err := json.Unmarshal(evt.Payload, &e); err != nil {
			return fmt.Errorf("%w: decode identity.user.deleted.v1: %w", eventbus.ErrPoison, err)
		}
		if e.UserID == "" || e.Pseudonym == "" {
			return fmt.Errorf("%w: identity.user.deleted.v1 without user_id or pseudonym", eventbus.ErrPoison)
		}
		return fn(ctx, evt.Envelope.TenantID, e)
	}
}
