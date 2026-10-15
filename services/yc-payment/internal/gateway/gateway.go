// Package gateway is the driver interface yc-payment's gateways implement
// (ADR-013). Drivers hold no state: credentials come with each call.
package gateway

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// Credentials are a shop's secrets for one gateway.
type Credentials struct {
	APIKey        string `json:"api_key"`
	WebhookSecret string `json:"webhook_secret,omitempty"`
}

// Status is a payment's state at the gateway.
type Status string

// Payment states.
const (
	StatusPending    Status = "pending"    // needs customer action or confirms later
	StatusAuthorized Status = "authorized" // held, not yet captured
	StatusCaptured   Status = "captured"
	StatusVoided     Status = "voided"
	StatusFailed     Status = "failed"
)

// Payment is a gateway's view of a payment.
type Payment struct {
	Reference string
	Status    Status
	// NextAction is what the customer must do (3-D Secure) while pending,
	// as the gateway describes it.
	NextAction    map[string]any
	FailureReason string
	// AmountCaptured in minor units, once captured.
	AmountCaptured int64
}

// AuthorizeRequest holds funds without capturing them.
type AuthorizeRequest struct {
	AmountMinor int64
	Currency    string
	// PaymentToken is the gateway SDK's token for the customer's payment
	// method (ADR-009); never stored.
	PaymentToken string
	ReturnURL    string
	Description  string
	Metadata     map[string]string
	// IdempotencyKey makes a repeated call return the first payment.
	IdempotencyKey string
}

// ChargeRequest takes a payment in one step, authorizing and capturing
// together (Variant B).
type ChargeRequest struct {
	AmountMinor int64
	Currency    string
	// PaymentToken is the gateway SDK's payment method; empty for a hosted
	// payment, where the gateway's own page collects it.
	PaymentToken string
	// ReturnURL is where the customer comes back to after a redirect.
	ReturnURL      string
	Description    string
	Metadata       map[string]string
	IdempotencyKey string
}

// RefundStatus is a refund's state at the gateway.
type RefundStatus string

// Refund states.
const (
	RefundPending   RefundStatus = "pending"
	RefundCompleted RefundStatus = "completed"
	RefundFailed    RefundStatus = "failed"
)

// Refund is a gateway's view of a refund.
type Refund struct {
	Reference     string
	Status        RefundStatus
	FailureReason string
}

// Dispute is a chargeback the gateway reports on a payment.
type Dispute struct {
	Reference   string
	AmountMinor int64
	Currency    string
	// Reason is the gateway's reason code, e.g. fraudulent.
	Reason string
}

// WebhookKind classifies a webhook for yc-payment.
type WebhookKind string

// Webhook kinds; others are acknowledged and ignored.
const (
	WebhookPayment WebhookKind = "payment" // Payment holds the new state
	WebhookRefund  WebhookKind = "refund"  // Refund holds the new state
	WebhookDispute WebhookKind = "dispute" // Dispute holds a new chargeback
	WebhookOther   WebhookKind = "other"
)

// WebhookEvent is a verified, parsed webhook.
type WebhookEvent struct {
	ID      string
	Type    string
	Kind    WebhookKind
	Payment Payment
	Refund  Refund
	Dispute Dispute
	// PaymentReference is the payment a refund or dispute belongs to.
	PaymentReference string
	// RefundAmount identifies a refund the gateway names differently from
	// its refund call's answer, when non-zero.
	RefundAmount int64
	Raw          []byte
}

// ErrSignature means a webhook's signature is missing, wrong or stale.
var ErrSignature = errors.New("gateway: webhook signature invalid")

// ErrUnsupported means the gateway has no such operation, e.g. Yoco cannot
// look up or cancel a checkout.
var ErrUnsupported = errors.New("gateway: operation not supported")

// Driver is one payment gateway.
type Driver interface {
	Name() string
	// SupportsSeparateAuthCapture selects the saga: Variant A when true,
	// Variant B (Charge) when false (ADR-013).
	SupportsSeparateAuthCapture() bool
	// SupportedCurrencies lists the ISO 4217 codes the gateway takes; nil
	// means any.
	SupportedCurrencies() []string
	// HostedPayment reports that the customer pays on the gateway's own
	// page: no payment token, and the outcome arrives by webhook.
	HostedPayment() bool
	Authorize(ctx context.Context, c Credentials, req AuthorizeRequest) (Payment, error)
	Charge(ctx context.Context, c Credentials, req ChargeRequest) (Payment, error)
	// Get returns a payment's current state, e.g. after 3-D Secure, or
	// ErrUnsupported.
	Get(ctx context.Context, c Credentials, reference string) (Payment, error)
	Capture(ctx context.Context, c Credentials, reference string, amountMinor int64, idempotencyKey string) (Payment, error)
	// Void cancels an authorization or an unfinished charge, or returns
	// ErrUnsupported.
	Void(ctx context.Context, c Credentials, reference, idempotencyKey string) (Payment, error)
	Refund(ctx context.Context, c Credentials, reference string, amountMinor int64, idempotencyKey string) (Refund, error)
	// ParseWebhook verifies a webhook against the shop's secret.
	ParseWebhook(secret string, header http.Header, body []byte, now time.Time) (WebhookEvent, error)
	// Ping checks the credentials.
	Ping(ctx context.Context, c Credentials) error
}

// DeclineError is a definite refusal by the gateway (card declined,
// invalid request), as opposed to an outage worth retrying.
type DeclineError struct {
	Code    string
	Message string
	// Payment is the gateway's payment, when it reported one.
	Payment *Payment
}

func (e *DeclineError) Error() string { return "gateway declined: " + e.Code + ": " + e.Message }
