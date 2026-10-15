package yoco

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/slaghuis/yc-platform/services/yc-payment/internal/gateway"
)

// Standard Webhooks headers (https://www.standardwebhooks.com).
const (
	IDHeader        = "webhook-id"
	TimestampHeader = "webhook-timestamp"
	SignatureHeader = "webhook-signature"
)

// Tolerance is how old a signed webhook may be (Yoco: three minutes).
const Tolerance = 3 * time.Minute

// secretPrefix starts a Yoco webhook secret; the rest is base64.
const secretPrefix = "whsec_"

// Sign returns a webhook-signature value for payload, as Yoco makes it.
func Sign(secret, id string, at time.Time, payload []byte) (string, error) {
	key, err := secretKey(secret)
	if err != nil {
		return "", err
	}
	return "v1," + signature(key, id, strconv.FormatInt(at.Unix(), 10), payload), nil
}

func secretKey(secret string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, secretPrefix))
}

func signature(key []byte, id, ts string, payload []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + ts + "."))
	mac.Write(payload)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// verify checks the Standard Webhooks headers: HMAC-SHA256 of
// "id.timestamp.payload" under the decoded secret, any v1 matching, the
// timestamp within Tolerance.
func verify(secret string, h http.Header, payload []byte, now time.Time) error {
	id, ts, sigs := h.Get(IDHeader), h.Get(TimestampHeader), h.Get(SignatureHeader)
	if secret == "" || id == "" || ts == "" || sigs == "" {
		return gateway.ErrSignature
	}
	key, err := secretKey(secret)
	if err != nil || len(key) == 0 {
		return fmt.Errorf("%w: webhook secret is not whsec_ base64", gateway.ErrSignature)
	}
	at, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return gateway.ErrSignature
	}
	if age := now.Sub(time.Unix(at, 0)); age > Tolerance || age < -Tolerance {
		return fmt.Errorf("%w: timestamp outside tolerance", gateway.ErrSignature)
	}
	want := signature(key, id, ts, payload)
	for s := range strings.FieldsSeq(sigs) {
		version, sig, ok := strings.Cut(s, ",")
		if ok && version == "v1" && hmac.Equal([]byte(sig), []byte(want)) {
			return nil
		}
	}
	return gateway.ErrSignature
}

type event struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Payload struct {
		ID            string            `json:"id"`
		Amount        int64             `json:"amount"`
		Status        string            `json:"status"`
		FailureReason string            `json:"failureReason"`
		Metadata      map[string]string `json:"metadata"`
	} `json:"payload"`
}

// ParseWebhook implements gateway.Driver. Payment and refund events name
// their checkout in metadata.checkoutId.
func (*Driver) ParseWebhook(secret string, header http.Header, body []byte, now time.Time) (gateway.WebhookEvent, error) {
	if err := verify(secret, header, body, now); err != nil {
		return gateway.WebhookEvent{}, err
	}
	var e event
	if err := json.Unmarshal(body, &e); err != nil || e.ID == "" {
		return gateway.WebhookEvent{}, fmt.Errorf("yoco: malformed event: %w", err)
	}
	out := gateway.WebhookEvent{
		ID: e.ID, Type: e.Type, Kind: gateway.WebhookOther, Raw: body,
		PaymentReference: e.Payload.Metadata["checkoutId"],
	}
	switch e.Type {
	case "payment.succeeded":
		out.Kind = gateway.WebhookPayment
		out.Payment = gateway.Payment{
			Reference: out.PaymentReference, Status: gateway.StatusCaptured,
			AmountCaptured: e.Payload.Amount,
		}
	case "payment.failed":
		out.Kind = gateway.WebhookPayment
		out.Payment = gateway.Payment{
			Reference: out.PaymentReference, Status: gateway.StatusFailed,
			FailureReason: "payment_failed",
		}
	case "refund.succeeded":
		out.Kind = gateway.WebhookRefund
		out.Refund = gateway.Refund{Reference: e.Payload.ID, Status: gateway.RefundCompleted}
		out.RefundAmount = e.Payload.Amount
	case "refund.failed":
		out.Kind = gateway.WebhookRefund
		out.Refund = gateway.Refund{
			Reference: e.Payload.ID, Status: gateway.RefundFailed,
			FailureReason: e.Payload.FailureReason,
		}
		out.RefundAmount = e.Payload.Amount
	}
	if out.Kind == gateway.WebhookPayment && out.PaymentReference == "" {
		return gateway.WebhookEvent{}, fmt.Errorf("yoco: %s without metadata.checkoutId", e.Type)
	}
	return out, nil
}
