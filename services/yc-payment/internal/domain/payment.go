// Package domain holds yc-payment's transactions, refunds and gateway
// settings, and the rules for moving between their states.
package domain

import (
	"strings"
	"time"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"

	"github.com/slaghuis/yc-platform/services/yc-payment/internal/gateway"
)

// Status is a transaction's state.
type Status string

// Transaction states. created means the gateway's answer is not known
// yet (the call failed or is in flight); a retry with the same key asks
// again.
const (
	StatusCreated    Status = "created"
	StatusPending    Status = "pending"
	StatusAuthorized Status = "authorized"
	StatusCaptured   Status = "captured"
	StatusVoided     Status = "voided"
	StatusRefunded   Status = "refunded"
	StatusFailed     Status = "failed"
)

// Settled reports whether the gateway has given a final answer.
func (s Status) Settled() bool {
	switch s {
	case StatusCreated, StatusPending:
		return false
	case StatusAuthorized, StatusCaptured, StatusVoided, StatusRefunded, StatusFailed:
		return true
	}
	return false
}

// Transaction is one payment.
type Transaction struct {
	ID               string
	SessionID        string
	OrderID          string
	Gateway          string
	GatewayReference string
	Status           Status
	AmountMinor      int64
	Currency         string
	CapturedMinor    int64
	RefundedMinor    int64
	IdempotencyKey   string
	NextAction       map[string]any
	FailureReason    string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Apply records the gateway's view of the payment. It never moves a
// transaction backwards: a late webhook for an older state is ignored.
// It reports whether anything changed.
func (t *Transaction) Apply(p gateway.Payment, now time.Time) bool {
	before := *t
	if p.Reference != "" {
		t.GatewayReference = p.Reference
	}
	next := Status(p.Status)
	if rank(next) > rank(t.Status) || (t.Status == StatusCreated && next != "") {
		t.Status = next
	}
	switch t.Status {
	case StatusPending:
		t.NextAction = p.NextAction
	case StatusFailed:
		t.NextAction = nil
		if t.FailureReason == "" {
			t.FailureReason = p.FailureReason
		}
	case StatusCaptured:
		t.NextAction = nil
		if p.AmountCaptured > 0 {
			t.CapturedMinor = p.AmountCaptured
		} else if t.CapturedMinor == 0 {
			t.CapturedMinor = t.AmountMinor
		}
	case StatusCreated, StatusAuthorized, StatusVoided, StatusRefunded:
		t.NextAction = nil
	}
	changed := t.Status != before.Status || t.GatewayReference != before.GatewayReference ||
		t.CapturedMinor != before.CapturedMinor || t.FailureReason != before.FailureReason
	if changed {
		t.UpdatedAt = now
	}
	return changed
}

// rank orders states by how far a payment has progressed; final states
// share the top ranks.
func rank(s Status) int {
	switch s {
	case StatusCreated:
		return 0
	case StatusPending:
		return 1
	case StatusAuthorized:
		return 2
	case StatusCaptured, StatusVoided, StatusFailed:
		return 3
	case StatusRefunded:
		return 4
	}
	return -1
}

// Refundable returns how much of the transaction may still be refunded.
func (t Transaction) Refundable() int64 {
	if t.Status != StatusCaptured {
		return 0
	}
	return t.CapturedMinor - t.RefundedMinor
}

// AddRefund counts a completed refund.
func (t *Transaction) AddRefund(amount int64, now time.Time) {
	t.RefundedMinor += amount
	if t.RefundedMinor >= t.CapturedMinor {
		t.Status = StatusRefunded
	}
	t.UpdatedAt = now
}

// RefundStatus is a refund's state.
type RefundStatus string

// Refund states.
const (
	RefundPending   RefundStatus = "pending"
	RefundCompleted RefundStatus = "completed"
	RefundFailed    RefundStatus = "failed"
)

// Refund is a refund of part or all of a captured transaction.
type Refund struct {
	ID               string
	TransactionID    string
	OrderID          string
	AmountMinor      int64
	Reason           string
	GatewayReference string
	Status           RefundStatus
	FailureReason    string
	ActorID          string
	IdempotencyKey   string
	CreatedAt        time.Time
	CompletedAt      *time.Time
}

// GatewayConfig is a shop's settings for one gateway, without secrets.
type GatewayConfig struct {
	Gateway          string
	Enabled          bool
	APIKeyHint       string
	WebhookSecretSet bool
	PublicKey        string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// KeyHint masks a secret for display: its prefix and last four characters.
func KeyHint(key string) string {
	prefix := ""
	if i := strings.LastIndex(key, "_"); i > 0 && i < len(key)-4 {
		prefix = key[:i+1]
	}
	if len(key) <= 8 {
		return prefix + "…"
	}
	return prefix + "…" + key[len(key)-4:]
}

// ValidateCredentials checks a shop's Stripe-style credentials.
func ValidateCredentials(c gateway.Credentials, publicKey string) error {
	v := ycerrors.Validation()
	if strings.TrimSpace(c.APIKey) == "" {
		v.Field("api_key", "required", "API key is required")
	} else if len(c.APIKey) > 255 || strings.ContainsAny(c.APIKey, " \t\r\n") {
		v.Field("api_key", "invalid", "API key is not valid")
	}
	if len(c.WebhookSecret) > 255 || strings.ContainsAny(c.WebhookSecret, " \t\r\n") {
		v.Field("webhook_secret", "invalid", "Webhook secret is not valid")
	}
	if len(publicKey) > 255 {
		v.Field("public_key", "too_long", "Public key is at most 255 characters")
	}
	if v.HasErrors() {
		return v.Build()
	}
	return nil
}

// MaxReason bounds free-text reasons.
const MaxReason = 500
