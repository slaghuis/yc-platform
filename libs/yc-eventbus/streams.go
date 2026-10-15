package eventbus

import (
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Retention per domain (ADR-004). Zero means unlimited.
const (
	retention7Years = 7 * 365 * 24 * time.Hour
	retention2Years = 2 * 365 * 24 * time.Hour
	retentionYear   = 365 * 24 * time.Hour
	retentionDLQ    = 90 * 24 * time.Hour
	retentionOps    = 90 * 24 * time.Hour
	retentionMonth  = 30 * 24 * time.Hour
	retentionWeek   = 7 * 24 * time.Hour
)

// DLQStreamName captures every dead-letter subject, yc.<tenant>.<domain>.dlq.v1.
const DLQStreamName = "YC_DLQ"

// DLQStream returns the stream for every tenant's and domain's DLQ subject,
// with the 90-day retention of ADR-003 and ADR-004.
func DLQStream() StreamConfig {
	return StreamConfig{
		Name:     DLQStreamName,
		Subjects: []string{"yc.*.*.dlq.v1"},
		Storage:  jetstream.FileStorage,
		MaxAge:   retentionDLQ,
	}
}

// DomainStream returns the stream for one domain's events, named
// YC_<DOMAIN>.
//
// JetStream refuses streams with overlapping subjects, and a domain-wide
// "yc.*.<domain>.>" would overlap the DLQ stream. The stream therefore
// captures four-segment event types (<domain>.<entity>.<action>.v<N>) by
// wildcard and three-segment ones (<domain>.<action>.v<N>) by name: pass
// those as aggregateEvents, e.g. "order.placed.v1".
func DomainStream(domain string, maxAge time.Duration, aggregateEvents ...string) StreamConfig {
	subjects := make([]string, 0, 1+len(aggregateEvents))
	subjects = append(subjects, "yc.*."+domain+".*.*.*")
	for _, e := range aggregateEvents {
		subjects = append(subjects, "yc.*."+e)
	}
	return StreamConfig{
		Name:     "YC_" + strings.ToUpper(domain),
		Subjects: subjects,
		Storage:  jetstream.FileStorage,
		MaxAge:   maxAge,
	}
}

// IdentityStream is the identity domain (1-year retention, ADR-004) except
// identity.user.deleted.v1, which IdentityDeletedStream keeps for 7 years.
// Its events are listed rather than captured by wildcard, so add new
// identity events here.
func IdentityStream() StreamConfig {
	events := []string{
		"identity.visitor.created.v1",
		"identity.user.registered.v1", "identity.user.promoted.v1", "identity.user.demoted.v1",
		"identity.user.logged_in.v1", "identity.user.login_failed.v1",
		"identity.user.password_reset_requested.v1", "identity.user.password_changed.v1",
		"identity.user.suspended.v1",
		"identity.oauth_linked.v1", "identity.oauth_unlinked.v1",
	}
	subjects := make([]string, 0, len(events))
	for _, e := range events {
		subjects = append(subjects, "yc.*."+e)
	}
	return StreamConfig{
		Name:     "YC_IDENTITY",
		Subjects: subjects,
		Storage:  jetstream.FileStorage,
		MaxAge:   retentionYear,
	}
}

// IdentityDeletedStream holds identity.user.deleted.v1 for 7 years, the
// proof of GDPR deletions (ADR-004, ADR-005). List it after IdentityStream:
// a YC_IDENTITY created before Phase 8 captured every identity event, and
// JetStream refuses overlapping streams until it is updated.
func IdentityDeletedStream() StreamConfig {
	return StreamConfig{
		Name:     "YC_IDENTITY_DELETED",
		Subjects: []string{"yc.*.identity.user.deleted.v1"},
		Storage:  jetstream.FileStorage,
		MaxAge:   retention7Years,
	}
}

// GDPRStreams are the streams a GDPR cascade participant needs: deletions
// in, platform.gdpr.cascade_completed.v1 out (ADR-005).
func GDPRStreams() []StreamConfig {
	return []StreamConfig{IdentityStream(), IdentityDeletedStream(), PlatformStream()}
}

// AuditStream is the audit domain (unlimited retention, ADR-004).
func AuditStream() StreamConfig { return DomainStream("audit", 0) }

// MonitorStream is the monitor domain (30-day retention, ADR-004).
func MonitorStream() StreamConfig { return DomainStream("monitor", retentionMonth) }

// AnalyticsStream is the analytics domain (1-year retention, ADR-004).
func AnalyticsStream() StreamConfig { return DomainStream("analytics", retentionYear) }

// CartStream is the cart domain (90-day retention, ADR-004).
func CartStream() StreamConfig {
	return DomainStream("cart", retentionOps,
		"cart.created.v1", "cart.cleared.v1", "cart.merged.v1", "cart.converted.v1", "cart.abandoned.v1")
}

// PreferenceStream is the preference domain (90-day retention, ADR-004).
func PreferenceStream() StreamConfig { return DomainStream("preference", retentionOps) }

// PlatformStream is the platform domain (unlimited retention, ADR-004).
func PlatformStream() StreamConfig { return DomainStream("platform", 0) }

// ProductStream is the product domain (1-year retention, ADR-004).
func ProductStream() StreamConfig {
	return DomainStream("product", retentionYear,
		"product.created.v1", "product.updated.v1", "product.deleted.v1",
		"product.image_attached.v1", "product.image_detached.v1")
}

// PricingStream is the pricing domain (1-year retention, ADR-004).
func PricingStream() StreamConfig {
	return DomainStream("pricing", retentionYear,
		"pricing.exchange_rates_updated.v1", "pricing.exchange_rates_stale.v1")
}

// ImageStream is the image domain (7-day retention, ADR-004). Every image
// event names the aggregate only, so all are listed.
func ImageStream() StreamConfig {
	return DomainStream("image", retentionWeek,
		"image.uploaded.v1", "image.processed.v1", "image.processing_failed.v1", "image.deleted.v1")
}

// InventoryStream is the inventory domain (1-year retention, ADR-004).
func InventoryStream() StreamConfig { return DomainStream("inventory", retentionYear) }

// PaymentStream is the payment domain (7-year retention, ADR-004).
func PaymentStream() StreamConfig {
	return DomainStream("payment", retention7Years,
		"payment.authorized.v1", "payment.authorization_failed.v1",
		"payment.captured.v1", "payment.capture_failed.v1",
		"payment.charged.v1", "payment.charge_failed.v1",
		"payment.voided.v1", "payment.void_failed.v1",
		"payment.refund_initiated.v1", "payment.refund_completed.v1", "payment.refund_failed.v1",
		"payment.chargeback_received.v1", "payment.webhook_signature_failed.v1")
}

// OrderStream is the order domain (7-year retention, ADR-004).
func OrderStream() StreamConfig {
	return DomainStream("order", retention7Years,
		"order.placed.v1", "order.packaged.v1", "order.shipped.v1", "order.delivered.v1",
		"order.cancelled.v1", "order.cancellation_failed.v1",
		"order.refund_requested.v1", "order.refunded.v1",
		"order.return_requested.v1", "order.return_accepted.v1", "order.return_rejected.v1", "order.return_received.v1",
		"order.disputed.v1", "order.note_added.v1")
}

// ShippingStream is the shipping domain (2-year retention, ADR-004).
func ShippingStream() StreamConfig {
	return DomainStream("shipping", retention2Years,
		"shipping.rate_quoted.v1", "shipping.label_created.v1", "shipping.label_failed.v1",
		"shipping.carrier_pickup.v1", "shipping.in_transit.v1", "shipping.out_for_delivery.v1",
		"shipping.delivered.v1", "shipping.delivery_exception.v1", "shipping.cancelled.v1")
}

// CheckoutStream is the checkout domain (7-year retention, ADR-004). Every
// checkout event names the aggregate only, so all are listed.
func CheckoutStream() StreamConfig {
	return DomainStream("checkout", retention7Years,
		"checkout.started.v1", "checkout.priced.v1", "checkout.shipping_selected.v1",
		"checkout.confirmed.v1", "checkout.validation_failed.v1", "checkout.compensating.v1",
		"checkout.completed.v1", "checkout.failed.v1", "checkout.expired.v1")
}

// MessageStream is the message domain (1-year retention, ADR-004).
func MessageStream() StreamConfig {
	return DomainStream("message", retentionYear,
		"message.queued.v1", "message.sent.v1", "message.delivered.v1", "message.opened.v1",
		"message.bounced.v1", "message.failed.v1",
		"message.verification_sent.v1", "message.verification_confirmed.v1")
}

// ReviewStream is the review domain (2-year retention, ADR-004). The
// review.like.* events have four segments; the rest are listed.
func ReviewStream() StreamConfig {
	return DomainStream("review", retention2Years,
		"review.submitted.v1", "review.approved.v1", "review.rejected.v1", "review.edited.v1",
		"review.deleted.v1", "review.responded.v1", "review.prompt_requested.v1")
}
