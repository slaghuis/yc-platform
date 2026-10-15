// Package yoco is the Yoco driver: hosted checkouts over the Checkout API
// (https://developer.yoco.com/api-reference/checkout-api), refunds and
// Standard Webhooks. Yoco takes the payment in one step (Variant B) on its
// own page; the outcome arrives only by webhook, because the Checkout API
// can neither look up nor cancel a checkout.
package yoco

import (
	"bytes"
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
const Name = "yoco"

// DefaultAPIBase is Yoco's Checkout API.
const DefaultAPIBase = "https://payments.yoco.com"

// maxBody bounds Yoco responses.
const maxBody = 1 << 20

// Driver calls the Yoco Checkout API.
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

// SupportsSeparateAuthCapture implements gateway.Driver: Yoco is Variant B.
func (*Driver) SupportsSeparateAuthCapture() bool { return false }

// SupportedCurrencies implements gateway.Driver: the Checkout API takes
// only ZAR.
func (*Driver) SupportedCurrencies() []string { return []string{"ZAR"} }

// HostedPayment implements gateway.Driver: the customer pays on Yoco's
// page.
func (*Driver) HostedPayment() bool { return true }

// checkout is the part of a Yoco checkout yc-payment reads.
type checkout struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	RedirectURL string `json:"redirectUrl"`
	SuccessURL  string `json:"successUrl"`
	PaymentID   string `json:"paymentId"`
	Amount      int64  `json:"amount"`
}

// payment maps a checkout to yc-payment's states. Until Yoco reports the
// payment by webhook the customer still has to pay on redirectUrl.
func (c checkout) payment() gateway.Payment {
	p := gateway.Payment{Reference: c.ID}
	if c.Status == "completed" {
		p.Status, p.AmountCaptured = gateway.StatusCaptured, c.Amount
		return p
	}
	p.Status = gateway.StatusPending
	p.NextAction = map[string]any{
		"type":            "redirect_to_url",
		"redirect_to_url": map[string]any{"url": c.RedirectURL, "return_url": c.SuccessURL},
	}
	return p
}

// Charge implements gateway.Driver: creates a hosted checkout for the
// customer to pay on. Yoco's idempotency makes a retried call return the
// same checkout.
func (d *Driver) Charge(ctx context.Context, c gateway.Credentials, req gateway.ChargeRequest) (gateway.Payment, error) {
	if req.PaymentToken != "" {
		return gateway.Payment{}, &gateway.DeclineError{
			Code:    "payment_token_unsupported",
			Message: "Yoco collects the payment method on its own page",
		}
	}
	in := map[string]any{"amount": req.AmountMinor, "currency": req.Currency}
	if req.ReturnURL != "" {
		in["successUrl"], in["cancelUrl"], in["failureUrl"] = req.ReturnURL, req.ReturnURL, req.ReturnURL
	}
	if len(req.Metadata) > 0 {
		in["metadata"] = req.Metadata
		if id := req.Metadata["transaction_id"]; id != "" {
			in["externalId"] = id
		}
	}
	var out checkout
	if err := d.call(ctx, c, http.MethodPost, "/api/checkouts", in, req.IdempotencyKey, &out); err != nil {
		return gateway.Payment{}, err
	}
	return out.payment(), nil
}

// Authorize implements gateway.Driver: Yoco has no separate authorization.
func (*Driver) Authorize(context.Context, gateway.Credentials, gateway.AuthorizeRequest) (gateway.Payment, error) {
	return gateway.Payment{}, gateway.ErrUnsupported
}

// Get implements gateway.Driver: the Checkout API cannot look a checkout
// up.
func (*Driver) Get(context.Context, gateway.Credentials, string) (gateway.Payment, error) {
	return gateway.Payment{}, gateway.ErrUnsupported
}

// Capture implements gateway.Driver: a Yoco payment is captured as it is
// made.
func (*Driver) Capture(context.Context, gateway.Credentials, string, int64, string) (gateway.Payment, error) {
	return gateway.Payment{}, gateway.ErrUnsupported
}

// Void implements gateway.Driver: an unpaid checkout cannot be cancelled;
// it stays payable on Yoco's page.
func (*Driver) Void(context.Context, gateway.Credentials, string, string) (gateway.Payment, error) {
	return gateway.Payment{}, gateway.ErrUnsupported
}

type refund struct {
	ID       string `json:"id"`
	RefundID string `json:"refundId"`
	Status   string `json:"status"`
	Message  string `json:"message"`
}

// Refund implements gateway.Driver: refunds a paid checkout, all of it or
// amountMinor.
func (d *Driver) Refund(ctx context.Context, c gateway.Credentials, reference string, amountMinor int64, key string) (gateway.Refund, error) {
	in := map[string]any{}
	if amountMinor > 0 {
		in["amount"] = amountMinor
	}
	var r refund
	if err := d.call(ctx, c, http.MethodPost, "/api/checkouts/"+url.PathEscape(reference)+"/refund", in, key, &r); err != nil {
		return gateway.Refund{}, err
	}
	out := gateway.Refund{Reference: r.RefundID, Status: gateway.RefundPending}
	switch r.Status {
	case "succeeded":
		out.Status = gateway.RefundCompleted
	case "failed":
		out.Status, out.FailureReason = gateway.RefundFailed, r.Message
	}
	return out, nil
}

// Ping implements gateway.Driver: lists the account's webhooks, which any
// valid secret key may.
func (d *Driver) Ping(ctx context.Context, c gateway.Credentials) error {
	return d.call(ctx, c, http.MethodGet, "/api/webhooks", nil, "", nil)
}

// apiError is Yoco's error body. The Checkout API answers with message,
// reason and solution; other Yoco APIs with errorCode and errorMessage.
type apiError struct {
	Message      string `json:"message"`
	Reason       string `json:"reason"`
	ErrorCode    string `json:"errorCode"`
	ErrorMessage string `json:"errorMessage"`
}

// call sends one request. A 4xx is a DeclineError (Yoco refused it and a
// retry will not help); network failures, 409 (the same key is still in
// flight), 429 and 5xx are gateway_unavailable.
func (d *Driver) call(ctx context.Context, c gateway.Credentials, method, path string, in any, idempotencyKey string, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("yoco: %w", err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.base+path, body)
	if err != nil {
		return fmt.Errorf("yoco: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return ycerrors.New(ycerrors.CodeGatewayUnavailable, "Yoco is unreachable").Wrap(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return ycerrors.New(ycerrors.CodeGatewayUnavailable, "reading Yoco's response").Wrap(err)
	}
	switch {
	case resp.StatusCode >= 500, resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode == http.StatusConflict:
		return ycerrors.New(ycerrors.CodeGatewayUnavailable, fmt.Sprintf("Yoco returned %d", resp.StatusCode))
	case resp.StatusCode >= 400:
		var e apiError
		_ = json.Unmarshal(raw, &e)
		de := &gateway.DeclineError{Code: e.Reason, Message: e.Message}
		if de.Code == "" {
			de.Code = e.ErrorCode
		}
		if de.Message == "" {
			de.Message = e.ErrorMessage
		}
		if de.Code == "" {
			de.Code = strconv.Itoa(resp.StatusCode)
		}
		return de
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return ycerrors.New(ycerrors.CodeGatewayUnavailable, "decoding Yoco's response").Wrap(err)
	}
	return nil
}
