// Package stripe is the Stripe driver: PaymentIntents with manual capture
// (Variant A), refunds and signed webhooks, over Stripe's form-encoded REST
// API.
package stripe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"

	"github.com/slaghuis/yc-platform/services/yc-payment/internal/gateway"
)

// Name is the driver's gateway name.
const Name = "stripe"

// DefaultAPIBase is Stripe's API.
const DefaultAPIBase = "https://api.stripe.com"

// maxBody bounds Stripe responses.
const maxBody = 1 << 20

// Driver calls the Stripe API.
type Driver struct {
	base   string
	client *http.Client
}

var _ gateway.Driver = (*Driver)(nil)

// New returns a driver for apiBase (DefaultAPIBase, or a fake in tests).
func New(apiBase string, client *http.Client) *Driver {
	if apiBase == "" {
		apiBase = DefaultAPIBase
	}
	return &Driver{base: strings.TrimRight(apiBase, "/"), client: client}
}

// Name implements gateway.Driver.
func (*Driver) Name() string { return Name }

// SupportsSeparateAuthCapture implements gateway.Driver: Stripe is Variant A.
func (*Driver) SupportsSeparateAuthCapture() bool { return true }

// SupportedCurrencies implements gateway.Driver: Stripe takes any currency
// the account settles.
func (*Driver) SupportedCurrencies() []string { return nil }

// HostedPayment implements gateway.Driver: the storefront collects the
// payment method with Stripe's SDK.
func (*Driver) HostedPayment() bool { return false }

// paymentIntent is the part of a Stripe PaymentIntent yc-payment reads.
type paymentIntent struct {
	ID               string         `json:"id"`
	Status           string         `json:"status"`
	AmountReceived   int64          `json:"amount_received"`
	NextAction       map[string]any `json:"next_action"`
	LastPaymentError *apiError      `json:"last_payment_error"`
}

type refund struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	FailureReason string `json:"failure_reason"`
	PaymentIntent string `json:"payment_intent"`
}

type apiError struct {
	Type          string         `json:"type"`
	Code          string         `json:"code"`
	DeclineCode   string         `json:"decline_code"`
	Message       string         `json:"message"`
	PaymentIntent *paymentIntent `json:"payment_intent"`
}

func (e *apiError) reason() string {
	switch {
	case e.DeclineCode != "":
		return e.DeclineCode
	case e.Code != "":
		return e.Code
	default:
		return e.Type
	}
}

// payment maps a PaymentIntent to yc-payment's states.
func (pi paymentIntent) payment() gateway.Payment {
	p := gateway.Payment{Reference: pi.ID, AmountCaptured: pi.AmountReceived}
	switch pi.Status {
	case "requires_capture":
		p.Status = gateway.StatusAuthorized
	case "succeeded":
		p.Status = gateway.StatusCaptured
	case "canceled":
		p.Status = gateway.StatusVoided
	case "requires_payment_method":
		// A declined confirmation returns the intent to this state.
		p.Status = gateway.StatusFailed
		p.FailureReason = "payment_method_required"
		if pi.LastPaymentError != nil {
			p.FailureReason = pi.LastPaymentError.reason()
		}
	default: // requires_action, requires_confirmation, processing
		p.Status = gateway.StatusPending
		p.NextAction = pi.NextAction
	}
	return p
}

// Authorize implements gateway.Driver: creates and confirms a manual-capture
// PaymentIntent. Stripe's idempotency makes a retried call return the same
// intent.
func (d *Driver) Authorize(ctx context.Context, c gateway.Credentials, req gateway.AuthorizeRequest) (gateway.Payment, error) {
	return d.confirm(ctx, c, "manual", gateway.ChargeRequest(req))
}

// Charge implements gateway.Driver: creates and confirms an
// automatic-capture PaymentIntent.
func (d *Driver) Charge(ctx context.Context, c gateway.Credentials, req gateway.ChargeRequest) (gateway.Payment, error) {
	return d.confirm(ctx, c, "automatic", req)
}

func (d *Driver) confirm(ctx context.Context, c gateway.Credentials, captureMethod string, req gateway.ChargeRequest) (gateway.Payment, error) {
	form := url.Values{
		"amount":                             {strconv.FormatInt(req.AmountMinor, 10)},
		"currency":                           {strings.ToLower(req.Currency)},
		"capture_method":                     {captureMethod},
		"payment_method":                     {req.PaymentToken},
		"confirm":                            {"true"},
		"automatic_payment_methods[enabled]": {"true"},
	}
	if req.ReturnURL != "" {
		form.Set("return_url", req.ReturnURL)
	} else {
		form.Set("automatic_payment_methods[allow_redirects]", "never")
	}
	if req.Description != "" {
		form.Set("description", req.Description)
	}
	for k, v := range req.Metadata {
		form.Set("metadata["+k+"]", v)
	}
	var pi paymentIntent
	if err := d.call(ctx, c, http.MethodPost, "/v1/payment_intents", form, req.IdempotencyKey, &pi); err != nil {
		return gateway.Payment{}, err
	}
	return pi.payment(), nil
}

// Get implements gateway.Driver.
func (d *Driver) Get(ctx context.Context, c gateway.Credentials, reference string) (gateway.Payment, error) {
	var pi paymentIntent
	if err := d.call(ctx, c, http.MethodGet, "/v1/payment_intents/"+url.PathEscape(reference), nil, "", &pi); err != nil {
		return gateway.Payment{}, err
	}
	return pi.payment(), nil
}

// Capture implements gateway.Driver.
func (d *Driver) Capture(ctx context.Context, c gateway.Credentials, reference string, amountMinor int64, key string) (gateway.Payment, error) {
	form := url.Values{}
	if amountMinor > 0 {
		form.Set("amount_to_capture", strconv.FormatInt(amountMinor, 10))
	}
	var pi paymentIntent
	if err := d.call(ctx, c, http.MethodPost, "/v1/payment_intents/"+url.PathEscape(reference)+"/capture", form, key, &pi); err != nil {
		return gateway.Payment{}, err
	}
	return pi.payment(), nil
}

// Void implements gateway.Driver: cancels the PaymentIntent, releasing
// the hold.
func (d *Driver) Void(ctx context.Context, c gateway.Credentials, reference, key string) (gateway.Payment, error) {
	var pi paymentIntent
	if err := d.call(ctx, c, http.MethodPost, "/v1/payment_intents/"+url.PathEscape(reference)+"/cancel", url.Values{}, key, &pi); err != nil {
		return gateway.Payment{}, err
	}
	return pi.payment(), nil
}

// Refund implements gateway.Driver.
func (d *Driver) Refund(ctx context.Context, c gateway.Credentials, reference string, amountMinor int64, key string) (gateway.Refund, error) {
	form := url.Values{"payment_intent": {reference}}
	if amountMinor > 0 {
		form.Set("amount", strconv.FormatInt(amountMinor, 10))
	}
	var r refund
	if err := d.call(ctx, c, http.MethodPost, "/v1/refunds", form, key, &r); err != nil {
		return gateway.Refund{}, err
	}
	return r.refund(), nil
}

func (r refund) refund() gateway.Refund {
	out := gateway.Refund{Reference: r.ID, FailureReason: r.FailureReason}
	switch r.Status {
	case "succeeded":
		out.Status = gateway.RefundCompleted
	case "failed", "canceled":
		out.Status = gateway.RefundFailed
	default: // pending, requires_action
		out.Status = gateway.RefundPending
	}
	return out
}

// Ping implements gateway.Driver: reads the account balance, which any
// valid secret key may.
func (d *Driver) Ping(ctx context.Context, c gateway.Credentials) error {
	return d.call(ctx, c, http.MethodGet, "/v1/balance", nil, "", nil)
}

// call sends one request. A 4xx is a DeclineError (Stripe refused it and a
// retry will not help); network failures and 5xx are gateway_unavailable.
func (d *Driver) call(ctx context.Context, c gateway.Credentials, method, path string, form url.Values, idempotencyKey string, out any) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, d.base+path, body)
	if err != nil {
		return fmt.Errorf("stripe: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return ycerrors.New(ycerrors.CodeGatewayUnavailable, "Stripe is unreachable").Wrap(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return ycerrors.New(ycerrors.CodeGatewayUnavailable, "reading Stripe's response").Wrap(err)
	}
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		return ycerrors.New(ycerrors.CodeGatewayUnavailable, fmt.Sprintf("Stripe returned %d", resp.StatusCode))
	}
	if resp.StatusCode >= 400 {
		var envelope struct {
			Error apiError `json:"error"`
		}
		_ = json.Unmarshal(raw, &envelope)
		e := envelope.Error
		de := &gateway.DeclineError{Code: e.reason(), Message: e.Message}
		if de.Code == "" {
			de.Code = strconv.Itoa(resp.StatusCode)
		}
		if e.PaymentIntent != nil {
			p := e.PaymentIntent.payment()
			if p.Status == gateway.StatusFailed {
				p.FailureReason = de.Code
			}
			de.Payment = &p
		}
		return de
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return ycerrors.New(ycerrors.CodeGatewayUnavailable, "decoding Stripe's response").Wrap(err)
	}
	return nil
}
