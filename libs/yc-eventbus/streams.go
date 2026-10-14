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
func IdentityStream() StreamConfig { return DomainStream("identity", retentionYear) }

// PlatformStream is the platform domain (unlimited retention, ADR-004).
func PlatformStream() StreamConfig { return DomainStream("platform", 0) }
