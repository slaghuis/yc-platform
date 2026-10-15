package eventbus

import (
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Retention per domain (ADR-004). Zero means unlimited.
const (
	retentionYear = 365 * 24 * time.Hour
	retentionDLQ  = 90 * 24 * time.Hour
	retentionOps  = 90 * 24 * time.Hour
	retentionWeek = 7 * 24 * time.Hour
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

// IdentityStream is the identity domain (1-year retention, ADR-004).
//
// TODO(ADR-004): identity.user.deleted.v1 needs 7-year retention; move it
// to its own stream when yc-identity starts publishing it.
func IdentityStream() StreamConfig {
	return DomainStream("identity", retentionYear, "identity.oauth_linked.v1")
}

// CartStream is the cart domain (90-day retention, ADR-004).
func CartStream() StreamConfig {
	return DomainStream("cart", retentionOps, "cart.created.v1", "cart.cleared.v1", "cart.merged.v1")
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
