// Package domain holds yc-audit's types and rules: which events are
// audited and how, and the GDPR cascade register. It does no I/O.
package domain

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	events "github.com/slaghuis/yc-platform/libs/yc-events"
	money "github.com/slaghuis/yc-platform/libs/yc-money"
)

// Category groups audit records (ADR-025).
type Category string

// Categories.
const (
	CategoryRoleChange      Category = "role_change"
	CategoryTenantLifecycle Category = "tenant_lifecycle"
	CategoryConfigChange    Category = "config_change"
	CategoryGDPR            Category = "gdpr"
	CategoryFinancial       Category = "financial"
	CategorySecurity        Category = "security"
)

// Categories lists every category.
var Categories = []Category{
	CategoryRoleChange, CategoryTenantLifecycle, CategoryConfigChange, CategoryGDPR, CategoryFinancial, CategorySecurity,
}

// Valid reports whether c is a known category.
func (c Category) Valid() bool { return slices.Contains(Categories, c) }

// ShopVisible reports whether shop admins see c in their shop's log. The
// GDPR records name deleted users, so only system admins see them.
func (c Category) ShopVisible() bool { return c != CategoryGDPR }

// ShopCategories are the categories shop admins see.
func ShopCategories() []Category {
	return slices.DeleteFunc(slices.Clone(Categories), func(c Category) bool { return !c.ShopVisible() })
}

// Record is one audited event. Records are never changed.
type Record struct {
	ID            string
	TenantID      string
	Category      Category
	EventType     string
	EventID       string
	SourceService string
	ActorType     string
	ActorID       string
	// SubjectRef is what the event is about, e.g. "user:<id>".
	SubjectRef    string
	Summary       string
	CorrelationID string
	Payload       json.RawMessage
	EventTime     time.Time
	RecordedAt    time.Time
}

// Rule says how an event type becomes a record.
type Rule struct {
	Category Category
	// Describe returns the record's subject and a one-line summary.
	Describe func(p map[string]any) (subject, summary string)
}

// Rules are the audited event types.
var Rules = map[string]Rule{
	events.TypeIdentityUserPromotedV1: {CategoryRoleChange, func(p map[string]any) (string, string) {
		return "user:" + str(p, "user_id"), fmt.Sprintf("Role changed from %s to %s", str(p, "from_role"), str(p, "to_role"))
	}},
	events.TypePlatformTenantProvisionedV1: {CategoryTenantLifecycle, func(p map[string]any) (string, string) {
		return "tenant:" + str(p, "tenant_id"), fmt.Sprintf("Shop %q provisioned", str(p, "name"))
	}},
	events.TypePlatformTenantUpdatedV1: {CategoryTenantLifecycle, func(p map[string]any) (string, string) {
		return "tenant:" + str(p, "tenant_id"), fmt.Sprintf("Shop %q updated (primary %s)", str(p, "name"), str(p, "primary_hostname"))
	}},
	events.TypePlatformTenantSuspendedV1: {CategoryTenantLifecycle, func(p map[string]any) (string, string) {
		return "tenant:" + str(p, "tenant_id"), fmt.Sprintf("Shop suspended: %s", str(p, "reason"))
	}},
	events.TypePlatformTenantReinstatedV1: {CategoryTenantLifecycle, func(p map[string]any) (string, string) {
		return "tenant:" + str(p, "tenant_id"), "Shop reinstated"
	}},
	events.TypePlatformTenantDeletedV1: {CategoryTenantLifecycle, func(p map[string]any) (string, string) {
		return "tenant:" + str(p, "tenant_id"), "Shop deleted"
	}},
	events.TypePricingConfigChangedV1: {CategoryConfigChange, func(p map[string]any) (string, string) {
		return "pricing-config", fmt.Sprintf("Pricing settings changed: store currency %s, tax inclusive %v, jurisdiction %s",
			str(p, "store_currency"), p["is_tax_inclusive"], str(p, "default_jurisdiction"))
	}},
	events.TypeIdentityUserDeletedV1: {CategoryGDPR, func(p map[string]any) (string, string) {
		return "user:" + str(p, "user_id"), fmt.Sprintf("%s account deleted at the %s's request",
			titled(str(p, "role")), requester(str(p, "requested_by")))
	}},
	events.TypePlatformGdprCascadeCompletedV1: {CategoryGDPR, func(p map[string]any) (string, string) {
		return "user:" + str(p, "user_id"), fmt.Sprintf("%s %s %v records", str(p, "service"), str(p, "action"), p["record_count"])
	}},
	events.TypeAuditGdprCascadeCompleteV1: {CategoryGDPR, func(p map[string]any) (string, string) {
		return "user:" + str(p, "user_id"), "GDPR deletion complete in every service"
	}},
	events.TypeAuditGdprCascadeIncidentV1: {CategoryGDPR, func(p map[string]any) (string, string) {
		return "user:" + str(p, "user_id"), fmt.Sprintf("GDPR deletion incomplete at its deadline; missing %v", p["missing_services"])
	}},
	events.TypePaymentChargebackReceivedV1: {CategoryFinancial, func(p map[string]any) (string, string) {
		return "transaction:" + str(p, "transaction_id"), fmt.Sprintf("Chargeback of %s on a %s payment (%s)",
			amount(p["amount"]), str(p, "gateway"), orNone(str(p, "reason")))
	}},
	events.TypeOrderDisputedV1: {CategoryFinancial, func(p map[string]any) (string, string) {
		return "order:" + str(p, "order_id"), fmt.Sprintf("Order %s disputed for %s", str(p, "order_number"), amount(p["amount"]))
	}},
	events.TypeOrderRefundedV1: {CategoryFinancial, func(p map[string]any) (string, string) {
		return "order:" + str(p, "order_id"), fmt.Sprintf("Refunded %s on order %s: %s",
			amount(p["amount"]), str(p, "order_number"), str(p, "reason"))
	}},
	events.TypePaymentWebhookSignatureFailedV1: {CategorySecurity, func(p map[string]any) (string, string) {
		return "gateway:" + str(p, "gateway"), fmt.Sprintf("%s webhook rejected: %s", str(p, "gateway"), str(p, "reason"))
	}},
}

// AuditedTypes lists the audited event types in a stable order.
func AuditedTypes() []string {
	out := make([]string, 0, len(Rules))
	for t := range Rules {
		out = append(out, t)
	}
	slices.Sort(out)
	return out
}

func str(p map[string]any, k string) string {
	s, _ := p[k].(string)
	return s
}

func titled(s string) string {
	switch s {
	case "shop_admin":
		return "Shop admin"
	case "customer":
		return "Customer"
	}
	return s
}

func requester(s string) string {
	if s == "system_admin" {
		return "system admin"
	}
	return s
}

func orNone(s string) string {
	if s == "" {
		return "no reason given"
	}
	return s
}

func amount(v any) string {
	m, _ := v.(map[string]any)
	minor, _ := m["amount_minor"].(float64)
	currency, _ := m["currency"].(string)
	if currency == "" {
		return "an unknown amount"
	}
	return money.New(int64(minor), currency).String()
}
