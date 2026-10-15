package stripe

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/slaghuis/yc-platform/services/yc-payment/internal/gateway"
)

// SignatureHeader carries a webhook's signature.
const SignatureHeader = "Stripe-Signature"

// Tolerance is how old a signed webhook may be.
const Tolerance = 5 * time.Minute

// Sign returns a Stripe-Signature value for payload, as Stripe makes it.
func Sign(secret string, payload []byte, at time.Time) string {
	t := strconv.FormatInt(at.Unix(), 10)
	return "t=" + t + ",v1=" + signature(secret, t, payload)
}

func signature(secret, t string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(t))
	mac.Write([]byte("."))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// verify checks a Stripe-Signature header: HMAC-SHA256 of "t.payload"
// under the endpoint's secret, any v1 matching, t within Tolerance.
func verify(secret, header string, payload []byte, now time.Time) error {
	if secret == "" || header == "" {
		return gateway.ErrSignature
	}
	var t string
	var sigs []string
	for part := range strings.SplitSeq(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			t = v
		case "v1":
			sigs = append(sigs, v)
		}
	}
	ts, err := strconv.ParseInt(t, 10, 64)
	if err != nil || len(sigs) == 0 {
		return gateway.ErrSignature
	}
	if age := now.Sub(time.Unix(ts, 0)); age > Tolerance || age < -Tolerance {
		return fmt.Errorf("%w: timestamp outside tolerance", gateway.ErrSignature)
	}
	want := signature(secret, t, payload)
	for _, s := range sigs {
		if hmac.Equal([]byte(s), []byte(want)) {
			return nil
		}
	}
	return gateway.ErrSignature
}

type event struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

// ParseWebhook implements gateway.Driver.
func (*Driver) ParseWebhook(secret string, header http.Header, body []byte, now time.Time) (gateway.WebhookEvent, error) {
	if err := verify(secret, header.Get(SignatureHeader), body, now); err != nil {
		return gateway.WebhookEvent{}, err
	}
	var e event
	if err := json.Unmarshal(body, &e); err != nil || e.ID == "" {
		return gateway.WebhookEvent{}, fmt.Errorf("stripe: malformed event: %w", err)
	}
	out := gateway.WebhookEvent{ID: e.ID, Type: e.Type, Kind: gateway.WebhookOther, Raw: body}
	switch {
	case strings.HasPrefix(e.Type, "payment_intent."):
		var pi paymentIntent
		if err := json.Unmarshal(e.Data.Object, &pi); err != nil {
			return gateway.WebhookEvent{}, fmt.Errorf("stripe: malformed payment intent: %w", err)
		}
		out.Kind = gateway.WebhookPayment
		out.Payment = pi.payment()
		out.PaymentReference = pi.ID
	case strings.HasPrefix(e.Type, "refund.") || e.Type == "charge.refund.updated":
		var r refund
		if err := json.Unmarshal(e.Data.Object, &r); err != nil {
			return gateway.WebhookEvent{}, fmt.Errorf("stripe: malformed refund: %w", err)
		}
		out.Kind = gateway.WebhookRefund
		out.Refund = r.refund()
		out.PaymentReference = r.PaymentIntent
	case e.Type == "charge.dispute.created":
		var d dispute
		if err := json.Unmarshal(e.Data.Object, &d); err != nil {
			return gateway.WebhookEvent{}, fmt.Errorf("stripe: malformed dispute: %w", err)
		}
		if d.ID == "" || d.PaymentIntent == "" || d.Amount <= 0 {
			return gateway.WebhookEvent{}, errors.New("stripe: dispute without id, payment_intent or amount")
		}
		out.Kind = gateway.WebhookDispute
		out.Dispute = gateway.Dispute{
			Reference: d.ID, AmountMinor: d.Amount, Currency: strings.ToUpper(d.Currency), Reason: d.Reason,
		}
		out.PaymentReference = d.PaymentIntent
	}
	return out, nil
}

// dispute is Stripe's Dispute object.
type dispute struct {
	ID            string `json:"id"`
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency"`
	PaymentIntent string `json:"payment_intent"`
	Reason        string `json:"reason"`
}
