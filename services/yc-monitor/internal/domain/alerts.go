package domain

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Severity is an alert's urgency.
type Severity string

// Severities. Critical alerts also go to the critical webhook.
const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// Audiences an alert notifies (monitor.alert.opened.v1 notify).
const (
	NotifySystemAdmins = "system_admin"
	NotifyShopAdmins   = "shop_admin"
)

// RuleKind is how an alert ends.
type RuleKind int

// Rule kinds.
const (
	// Threshold alerts resolve when the measured value is back in bounds.
	Threshold RuleKind = iota
	// Health alerts resolve when the service recovers.
	Health
	// Incident alerts are raised by an event and resolve when acknowledged.
	Incident
)

// Alert rule names.
const (
	RuleDLQRate             = "dlq_rate"
	RuleCheckoutSuccessRate = "checkout_success_rate"
	RuleServiceUnhealthy    = "service_unhealthy"
	RuleGDPRIncident        = "gdpr_cascade_incident"
	RuleOrderDisputed       = "order_disputed"
	RuleWebhookSignature    = "webhook_signature_failed"
	RuleExchangeRatesStale  = "exchange_rates_stale"
)

// Rule is a built-in alert rule.
type Rule struct {
	Name     string
	Kind     RuleKind
	Metric   string
	Severity Severity
	Notify   []string
	// PlatformOnly rules are evaluated across shops and alert under
	// _platform; their thresholds cannot be set per shop.
	PlatformOnly bool
	// Below rules fire when the value drops under the threshold.
	Below bool
}

// Rules are the built-in rules by name.
var Rules = map[string]Rule{
	RuleDLQRate: {
		Name: RuleDLQRate, Kind: Threshold, Metric: "events.dlq.per_minute", Severity: SeverityWarning,
		Notify: []string{NotifySystemAdmins}, PlatformOnly: true,
	},
	RuleCheckoutSuccessRate: {
		Name: RuleCheckoutSuccessRate, Kind: Threshold, Metric: "checkouts.success_rate", Severity: SeverityWarning,
		Notify: []string{NotifySystemAdmins, NotifyShopAdmins}, Below: true,
	},
	RuleServiceUnhealthy: {
		Name: RuleServiceUnhealthy, Kind: Health, Metric: "service.up", Severity: SeverityCritical,
		Notify: []string{NotifySystemAdmins}, PlatformOnly: true,
	},
	RuleGDPRIncident: {
		Name: RuleGDPRIncident, Kind: Incident, Metric: "gdpr.cascade_incident", Severity: SeverityCritical,
		Notify: []string{NotifySystemAdmins}, PlatformOnly: true,
	},
	RuleOrderDisputed: {
		Name: RuleOrderDisputed, Kind: Incident, Metric: "orders.disputed", Severity: SeverityWarning,
		Notify: []string{NotifyShopAdmins},
	},
	RuleWebhookSignature: {
		Name: RuleWebhookSignature, Kind: Incident, Metric: "payments.webhook_signature_failed",
		Severity: SeverityWarning, Notify: []string{NotifySystemAdmins}, PlatformOnly: true,
	},
	RuleExchangeRatesStale: {
		Name: RuleExchangeRatesStale, Kind: Incident, Metric: "pricing.exchange_rates_stale",
		Severity: SeverityWarning, Notify: []string{NotifySystemAdmins}, PlatformOnly: true,
	},
}

// Breached reports whether value crosses threshold.
func (r Rule) Breached(value, threshold float64) bool {
	if r.Below {
		return value < threshold
	}
	return value > threshold
}

// Thresholds are a threshold rule's settings.
type Thresholds struct {
	TenantID      string     `json:"tenant_id"`
	Rule          string     `json:"rule"`
	Threshold     float64    `json:"threshold"`
	MinVolume     int64      `json:"min_volume"`
	WindowMinutes int        `json:"window_minutes"`
	Enabled       bool       `json:"enabled"`
	UpdatedBy     string     `json:"updated_by,omitempty"`
	UpdatedAt     *time.Time `json:"updated_at,omitempty"`
}

// Window is the evaluation window.
func (t Thresholds) Window() time.Duration { return time.Duration(t.WindowMinutes) * time.Minute }

// AlertStatus is derived from an alert's timestamps.
type AlertStatus string

// Alert statuses.
const (
	AlertOpen         AlertStatus = "open"
	AlertAcknowledged AlertStatus = "acknowledged"
	AlertResolved     AlertStatus = "resolved"
)

// Alert is a raised alert. There is at most one unresolved alert per
// tenant, rule and scope.
type Alert struct {
	ID             string
	TenantID       string
	Rule           string
	Metric         string
	Scope          string
	Dimensions     map[string]string
	Severity       Severity
	Summary        string
	Value          *float64
	Threshold      *float64
	Notify         []string
	Occurrences    int
	OpenedAt       time.Time
	LastSeenAt     time.Time
	AcknowledgedAt *time.Time
	AcknowledgedBy string
	ResolvedAt     *time.Time
}

// Status is the alert's status.
func (a Alert) Status() AlertStatus {
	switch {
	case a.ResolvedAt != nil:
		return AlertResolved
	case a.AcknowledgedAt != nil:
		return AlertAcknowledged
	default:
		return AlertOpen
	}
}

// Scope is dims as a stable string: sorted key=value pairs.
func Scope(dims map[string]string) string {
	keys := make([]string, 0, len(dims))
	for k := range dims {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + dims[k]
	}
	return strings.Join(parts, ",")
}

// AlertFilter narrows an alert listing. An empty TenantID lists every
// tenant.
type AlertFilter struct {
	TenantID string
	Status   AlertStatus
	Severity Severity
	Rule     string
}

// Percent formats a ratio as a percentage.
func Percent(v float64) string { return fmt.Sprintf("%.0f%%", v*100) }
