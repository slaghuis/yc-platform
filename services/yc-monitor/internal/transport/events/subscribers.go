// Package events holds yc-monitor's event subscribers.
package events

import (
	"context"
	"encoding/json"

	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	ycevents "github.com/slaghuis/yc-platform/libs/yc-events"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	"github.com/slaghuis/yc-platform/services/yc-monitor/internal/domain"
)

// Recorder stores what yc-monitor consumes.
type Recorder interface {
	RecordEvent(ctx context.Context, e domain.ReceivedEvent) error
	RecordDeadLetter(ctx context.Context, d domain.DLQEntry) error
}

// VisitorCreatedSubscription is every tenant's identity.visitor.created.v1.
var VisitorCreatedSubscription = eventbus.SubscribeOpts{
	Subject:     "yc.*." + ycevents.TypeIdentityVisitorCreatedV1,
	DurableName: "yc-monitor-visitor-created",
}

// DLQSubscription is every tenant's and domain's DLQ (ADR-003 rule 2).
var DLQSubscription = eventbus.SubscribeOpts{
	Subject:     "yc.*.*.dlq.v1",
	DurableName: "yc-monitor-dlq",
}

// Received records any consumed event.
func Received(r Recorder) eventbus.Handler {
	return func(ctx context.Context, evt eventbus.ConsumedEvent) error {
		env := evt.Envelope
		if err := r.RecordEvent(ctx, domain.ReceivedEvent{
			EventID:       env.EventID,
			TenantID:      env.TenantID,
			EventType:     env.EventType,
			SourceService: env.SourceService,
			CorrelationID: env.CorrelationID,
			EventTime:     env.EventTime,
			Envelope:      evt.RawEnvelope,
		}); err != nil {
			return err
		}
		telemetry.LoggerFromContext(ctx).Info("event recorded",
			"event_id", env.EventID, "event_type", env.EventType, "source_service", env.SourceService)
		return nil
	}
}

// DeadLetter records a dead letter.
func DeadLetter(r Recorder) eventbus.DLQHandler {
	return func(ctx context.Context, dl eventbus.DeadLetter) error {
		orig, meta := dl.OriginalEvent, dl.DLQMetadata
		envelope, err := json.Marshal(orig)
		if err != nil {
			return err
		}
		service := meta.Service
		if service == "" {
			service = "unknown"
		}
		telemetry.LoggerFromContext(ctx).Warn("dead letter recorded",
			"dlq_subject", dl.Subject, "dlq_sequence", dl.Sequence, "event_id", orig.EventID,
			"event_type", orig.EventType, "consumer", meta.Consumer, "failure_reason", meta.FailureReason)
		return r.RecordDeadLetter(ctx, domain.DLQEntry{
			DLQSequence:    dl.Sequence,
			TenantID:       dl.TenantID,
			Service:        service,
			Consumer:       meta.Consumer,
			EventType:      orig.EventType,
			EventID:        orig.EventID,
			FailureReason:  meta.FailureReason,
			Attempts:       meta.Attempts,
			LastError:      meta.LastError,
			FirstFailureAt: meta.FirstFailure,
			LastFailureAt:  meta.LastFailure,
			Envelope:       envelope,
		})
	}
}
