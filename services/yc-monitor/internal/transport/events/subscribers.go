// Package events holds yc-monitor's event subscribers.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	ycevents "github.com/slaghuis/yc-platform/libs/yc-events"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	"github.com/slaghuis/yc-platform/services/yc-monitor/internal/domain"
)

// Handlers are the use cases the subscribers drive.
type Handlers interface {
	RecordEvent(ctx context.Context, e domain.ReceivedEvent) error
	RecordDeadLetter(ctx context.Context, d domain.DLQEntry) error
	Count(ctx context.Context, env eventbus.Envelope, metric string, dims map[string]string) error
	ServiceRegistered(ctx context.Context, p ycevents.PlatformServiceRegisteredV1) error
	ServiceDeregistered(ctx context.Context, p ycevents.PlatformServiceDeregisteredV1) error
	OrderDisputed(ctx context.Context, env eventbus.Envelope, p ycevents.OrderDisputedV1) error
	GDPRIncident(ctx context.Context, env eventbus.Envelope, p ycevents.AuditGdprCascadeIncidentV1) error
	WebhookSignatureFailed(ctx context.Context, env eventbus.Envelope, p ycevents.PaymentWebhookSignatureFailedV1) error
	ExchangeRatesStale(ctx context.Context, env eventbus.Envelope, p ycevents.PricingExchangeRatesStaleV1) error
	CriticalWebhook(ctx context.Context, env eventbus.Envelope, p ycevents.MonitorAlertOpenedV1) error
}

// Subscription pairs a durable consumer with its handler.
type Subscription struct {
	Opts    eventbus.SubscribeOpts
	Handler eventbus.Handler
}

// DLQSubscription is every tenant's and domain's DLQ (ADR-003 rule 2).
var DLQSubscription = eventbus.SubscribeOpts{
	Subject:     "yc.*.*.dlq.v1",
	DurableName: "yc-monitor-dlq",
}

// counters maps the counted event types to their counter.
var counters = map[string]string{
	ycevents.TypeCheckoutStartedV1:            domain.MetricCheckoutsStarted,
	ycevents.TypeCheckoutCompletedV1:          domain.MetricCheckoutsCompleted,
	ycevents.TypeCheckoutFailedV1:             domain.MetricCheckoutsFailed,
	ycevents.TypeOrderPlacedV1:                domain.MetricOrdersPlaced,
	ycevents.TypePaymentAuthorizationFailedV1: domain.MetricPaymentsFailed,
	ycevents.TypePaymentChargeFailedV1:        domain.MetricPaymentsFailed,
	ycevents.TypePaymentCaptureFailedV1:       domain.MetricPaymentsFailed,
}

// Subscriptions are yc-monitor's consumers. The critical webhook consumes
// yc-monitor's own alerts only when a webhook is configured.
func Subscriptions(h Handlers, criticalWebhook bool) []Subscription {
	subs := []Subscription{
		sub(ycevents.TypeIdentityVisitorCreatedV1, Received(h)),
		sub(ycevents.TypePlatformServiceRegisteredV1, platform(h.ServiceRegistered)),
		sub(ycevents.TypePlatformServiceDeregisteredV1, platform(h.ServiceDeregistered)),
		sub(ycevents.TypeOrderDisputedV1, typed(h.OrderDisputed)),
		sub(ycevents.TypeAuditGdprCascadeIncidentV1, typed(h.GDPRIncident)),
		sub(ycevents.TypePaymentWebhookSignatureFailedV1, typed(h.WebhookSignatureFailed)),
		sub(ycevents.TypePricingExchangeRatesStaleV1, typed(h.ExchangeRatesStale)),
	}
	for typ, metric := range counters {
		subs = append(subs, sub(typ, count(h, metric)))
	}
	if criticalWebhook {
		s := sub(ycevents.TypeMonitorAlertOpenedV1, typed(h.CriticalWebhook))
		s.Opts.DurableName = "yc-monitor-critical-webhook"
		subs = append(subs, s)
	}
	return subs
}

func sub(eventType string, h eventbus.Handler) Subscription {
	return Subscription{
		Opts:    eventbus.SubscribeOpts{Subject: "yc.*." + eventType, DurableName: "yc-monitor-" + durable(eventType)},
		Handler: h,
	}
}

// durable names a consumer after its event type: order.placed.v1 becomes
// order-placed. identity.visitor.created.v1 keeps its Phase 1 name.
func durable(eventType string) string {
	name := strings.NewReplacer(".", "-", "_", "-").Replace(strings.TrimSuffix(eventType, ".v1"))
	return strings.TrimPrefix(name, "identity-")
}

func count(h Handlers, metric string) eventbus.Handler {
	return func(ctx context.Context, evt eventbus.ConsumedEvent) error {
		var dims map[string]string
		if metric == domain.MetricPaymentsFailed {
			var p struct {
				Gateway string `json:"gateway"`
			}
			if err := json.Unmarshal(evt.Payload, &p); err != nil {
				return fmt.Errorf("%w: decode payload: %w", eventbus.ErrPoison, err)
			}
			dims = map[string]string{"gateway": p.Gateway}
		}
		return poisonInvalid(h.Count(ctx, evt.Envelope, metric, dims))
	}
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

func platform[T any](fn func(ctx context.Context, p T) error) eventbus.Handler {
	return typed(func(ctx context.Context, _ eventbus.Envelope, p T) error { return fn(ctx, p) })
}

// poisonInvalid sends events with invalid content to the DLQ rather than
// retrying them.
func poisonInvalid(err error) error {
	if ycerrors.HasCode(err, ycerrors.CodeValidationFailed) {
		return fmt.Errorf("%w: %w", eventbus.ErrPoison, err)
	}
	return err
}

// Received records any consumed event (the Phase 1 event log).
func Received(r Handlers) eventbus.Handler {
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
func DeadLetter(r Handlers) eventbus.DLQHandler {
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
