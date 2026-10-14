package eventbus

import (
	"fmt"
	"regexp"
	"strings"
)

// PlatformTenantID is the reserved pseudo-tenant for platform events (ADR-008).
const PlatformTenantID = "_platform"

const maxTenantIDLen = 63

// tenantIDPattern admits lowercase slugs only. Dots, wildcards ('*', '>'),
// underscores and whitespace are excluded so a tenant ID always occupies
// exactly one subject segment and can never collide with _platform.
var tenantIDPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ValidateTenantID reports whether tenantID is PlatformTenantID or a
// lowercase slug of at most 63 characters (e.g. "shop-abc").
func ValidateTenantID(tenantID string) error {
	if tenantID == "" {
		return fmt.Errorf("eventbus: empty tenant_id")
	}
	if tenantID == PlatformTenantID {
		return nil
	}
	if len(tenantID) > maxTenantIDLen || !tenantIDPattern.MatchString(tenantID) {
		return fmt.Errorf("eventbus: tenant_id %q must be %q or a lowercase slug matching %s (max %d chars)",
			tenantID, PlatformTenantID, tenantIDPattern, maxTenantIDLen)
	}
	return nil
}

// TenantOfSubject returns the tenant segment of a "yc.<tenant_id>.…" subject.
func TenantOfSubject(subject string) (string, error) {
	parts := strings.SplitN(subject, ".", 3)
	if len(parts) < 3 || parts[0] != "yc" {
		return "", fmt.Errorf("eventbus: subject %q is not of the form yc.<tenant_id>.<event_type>", subject)
	}
	if err := ValidateTenantID(parts[1]); err != nil {
		return "", err
	}
	return parts[1], nil
}

// CheckSubjectTenant returns an ErrPoison-wrapped error unless envTenantID
// equals the tenant segment of the subject the event was delivered on.
// Subscribe applies it to every message before dedup and the handler.
func CheckSubjectTenant(subject, envTenantID string) error {
	subjectTenant, err := TenantOfSubject(subject)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPoison, err)
	}
	if envTenantID != subjectTenant {
		return fmt.Errorf("%w: envelope tenant_id %q does not match subject tenant %q",
			ErrPoison, envTenantID, subjectTenant)
	}
	return nil
}

// SubjectForEvent builds the NATS subject for publishing an event.
//
// Pattern: yc.<tenant_id>.<domain>[.<entity>].<action>.v<version>
//
// The tenant is always the second segment and must pass ValidateTenantID.
// The domain, entity, action, and version come from the EventType string,
// which must be formatted as "<domain>.<action>.v<N>" or
// "<domain>.<entity>.<action>.v<N>". The entity is omitted when it is the
// domain's own aggregate (order.placed.v1, not order.order.placed.v1).
//
// Example: SubjectForEvent("shop-abc", "order.placed.v1")
//          returns "yc.shop-abc.order.placed.v1"
func SubjectForEvent(tenantID, eventType string) (string, error) {
	if err := ValidateTenantID(tenantID); err != nil {
		return "", err
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
	if err := ValidateTenantID(tenantID); err != nil {
		return "", err
	}
	if domain == "" {
		return "", fmt.Errorf("eventbus: empty domain")
	}
	return fmt.Sprintf("yc.%s.%s.dlq.v1", tenantID, domain), nil
}

// DomainOfEventType returns the leading domain segment of an event type.
// For "order.placed.v1", returns "order".
func DomainOfEventType(eventType string) (string, error) {
	if err := validateEventType(eventType); err != nil {
		return "", err
	}
	return strings.SplitN(eventType, ".", 2)[0], nil
}

// validateEventType rejects event types that don't match the required
// shape of <domain>[.<entity>].<action>.v<N>. Enforced at the library edge
// so bad event types never land on the bus.
func validateEventType(eventType string) error {
	parts := strings.Split(eventType, ".")
	if len(parts) < 3 || len(parts) > 4 {
		return fmt.Errorf("eventbus: event_type %q must have 3 or 4 dot-separated segments", eventType)
	}
	if parts[0] == parts[1] {
		return fmt.Errorf("eventbus: event_type %q repeats the domain as the entity; omit the entity", eventType)
	}
	// Last segment must be v<N>.
	last := parts[len(parts)-1]
	if len(last) < 2 || last[0] != 'v' {
		return fmt.Errorf("eventbus: event_type %q must end with v<N>", eventType)
	}
	for _, c := range last[1:] {
		if c < '0' || c > '9' {
			return fmt.Errorf("eventbus: event_type %q must end with v<N>", eventType)
		}
	}
	for _, part := range parts {
		if part == "" {
			return fmt.Errorf("eventbus: event_type %q contains empty segment", eventType)
		}
	}
	return nil
}