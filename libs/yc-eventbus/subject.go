package eventbus

import (
	"fmt"
	"strings"
)

// SubjectForEvent builds the NATS subject for publishing an event.
//
// Pattern: yc.<tenant_id>.<domain>.<entity>.<action>.v<version>
//
// The domain, entity, action, and version come from the EventType string,
// which must be formatted as "<domain>.<entity>.<action>.v<N>".
//
// Example: SubjectForEvent("shop-abc", "order.order.placed.v1")
//          returns "yc.shop-abc.order.order.placed.v1"
func SubjectForEvent(tenantID, eventType string) (string, error) {
	if tenantID == "" {
		return "", fmt.Errorf("eventbus: empty tenant_id")
	}
	if err := validateEventType(eventType); err != nil {
		return "", err
	}
	return fmt.Sprintf("yc.%s.%s", tenantID, eventType), nil
}

// SubjectForDLQ builds the DLQ subject for a given tenant and domain.
//
// Pattern: yc.<tenant_id>.<domain>.dlq.v1
func SubjectForDLQ(tenantID, domain string) (string, error) {
	if tenantID == "" {
		return "", fmt.Errorf("eventbus: empty tenant_id")
	}
	if domain == "" {
		return "", fmt.Errorf("eventbus: empty domain")
	}
	return fmt.Sprintf("yc.%s.%s.dlq.v1", tenantID, domain), nil
}

// DomainOfEventType returns the leading domain segment of an event type.
// For "order.order.placed.v1", returns "order".
func DomainOfEventType(eventType string) (string, error) {
	if err := validateEventType(eventType); err != nil {
		return "", err
	}
	return strings.SplitN(eventType, ".", 2)[0], nil
}

// validateEventType rejects event types that don't match the required
// shape of <domain>.<entity>.<action>.v<N>. Enforced at the library edge
// so bad event types never land on the bus.
func validateEventType(eventType string) error {
	parts := strings.Split(eventType, ".")
	if len(parts) < 4 {
		return fmt.Errorf("eventbus: event_type %q must have at least 4 dot-separated segments", eventType)
	}
	// Last segment must be v<N>.
	last := parts[len(parts)-1]
	if len(last) < 2 || last[0] != 'v' {
		return fmt.Errorf("eventbus: event_type %q must end with v<N>", eventType)
	}
	for _, part := range parts {
		if part == "" {
			return fmt.Errorf("eventbus: event_type %q contains empty segment", eventType)
		}
	}
	return nil
}