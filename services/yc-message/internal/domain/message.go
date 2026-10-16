// Package domain holds yc-message's types and rules: message types,
// channels, recipient resolution, templates and verification codes. It does
// no I/O.
package domain

import (
	"slices"
	"strings"
	"time"
)

// Type is a message type: what a message is for. Each has a template per
// channel and locale.
type Type string

// The v1 message types (docs/services/yc-message.md, Example Message Types).
const (
	TypeWelcome             Type = "welcome"
	TypePasswordReset       Type = "password_reset"
	TypeChannelVerification Type = "channel_verification"
	TypeOrderConfirmation   Type = "order_confirmation"
	TypeOrderShipped        Type = "order_shipped"
	TypeOrderDelivered      Type = "order_delivered"
	TypeOrderCancelled      Type = "order_cancelled"
	TypeRefundIssued        Type = "refund_issued"
	TypeCartAbandoned       Type = "cart_abandoned"
	TypeReviewPrompt        Type = "review_prompt"
	// Phase 8: to admins rather than customers.
	TypeAdminAlert Type = "admin_alert"
	TypeMilestone  Type = "milestone"
	// Phase 12: invitation link (ADR-030); sent via POST /internal/send.
	TypeAdminInvite Type = "admin_invite"
)

// Types lists every message type, in a stable order.
var Types = []Type{
	TypeWelcome, TypePasswordReset, TypeChannelVerification,
	TypeOrderConfirmation, TypeOrderShipped, TypeOrderDelivered, TypeOrderCancelled, TypeRefundIssued,
	TypeCartAbandoned, TypeReviewPrompt, TypeAdminAlert, TypeMilestone, TypeAdminInvite,
}

// Valid reports whether t is a known message type.
func (t Type) Valid() bool { return slices.Contains(Types, t) }

// Sensitive types carry a secret (a reset link or a code). Their body and
// context are redacted once the message has been sent or has failed, so
// shop admins never see the secret.
func (t Type) Sensitive() bool {
	return t == TypePasswordReset || t == TypeChannelVerification || t == TypeAdminInvite
}

// Channel is how a message reaches its recipient.
type Channel string

// Channels. yc-preference accepts all four; v1 has drivers for email and
// SMS only.
const (
	ChannelEmail    Channel = "email"
	ChannelSMS      Channel = "sms"
	ChannelWhatsApp Channel = "whatsapp"
	ChannelTelegram Channel = "telegram"
)

// Valid reports whether c is a known channel.
func (c Channel) Valid() bool {
	switch c {
	case ChannelEmail, ChannelSMS, ChannelWhatsApp, ChannelTelegram:
		return true
	default:
		return false
	}
}

// Status is where a message is in its delivery.
type Status string

// Statuses (docs/services/yc-message.md, Delivery Status Tracking).
const (
	StatusQueued    Status = "queued"
	StatusSent      Status = "sent"
	StatusDelivered Status = "delivered"
	StatusBounced   Status = "bounced"
	StatusFailed    Status = "failed"
)

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	switch s {
	case StatusQueued, StatusSent, StatusDelivered, StatusBounced, StatusFailed:
		return true
	default:
		return false
	}
}

// Redacted replaces a sensitive message's body and context once it is no
// longer needed.
const Redacted = "[redacted]"

// Message is one message to one recipient on one channel.
type Message struct {
	ID       string
	TenantID string
	Type     Type
	// UserID is the recipient's user; "" for a guest or a plain address.
	UserID  string
	Address string
	Channel Channel
	Locale  string
	Rendered
	Status            Status
	Driver            string
	ProviderMessageID string
	Attempts          int
	LastError         string
	NextAttemptAt     *time.Time
	Context           map[string]any
	// Reference makes a send idempotent: one message per reference in a
	// shop, e.g. the triggering event's ID and the message type.
	Reference   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	SentAt      *time.Time
	DeliveredAt *time.Time
}

// Backoff is the delay before attempt n+1 after n failed attempts: base
// doubled per attempt, at most an hour.
func Backoff(base time.Duration, attempts int) time.Duration {
	d := base
	for i := 1; i < attempts && d < time.Hour; i++ {
		d *= 2
	}
	return min(d, time.Hour)
}

// Redact clears a sensitive message's secret.
func (m *Message) Redact() {
	if !m.Type.Sensitive() {
		return
	}
	m.Body = Redacted
	m.Context = map[string]any{"redacted": true}
}

// Language is the language part of a BCP 47 locale: "en-ZA" → "en".
func Language(locale string) string {
	lang, _, _ := strings.Cut(locale, "-")
	return strings.ToLower(lang)
}

// LocaleCandidates are the locales to look a template up in, most
// specific first: the locale, its language, then fallback.
func LocaleCandidates(locale, fallback string) []string {
	var out []string
	for _, l := range []string{locale, Language(locale), fallback} {
		if l != "" && !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	return out
}
