package yoco_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"

	"github.com/slaghuis/yc-platform/services/yc-payment/internal/gateway"
	"github.com/slaghuis/yc-platform/services/yc-payment/internal/gateway/yoco"
	"github.com/slaghuis/yc-platform/services/yc-payment/internal/gateway/yoco/yocotest"
)

const apiKey = "sk_test_yoco"

func setup(t *testing.T) (*yoco.Driver, *yocotest.Server) {
	t.Helper()
	fake := yocotest.NewServer(t)
	return yoco.New(fake.URL, http.DefaultClient), fake
}

func charge(t *testing.T, d *yoco.Driver, key string) (gateway.Payment, error) {
	t.Helper()
	return d.Charge(context.Background(), gateway.Credentials{APIKey: apiKey}, gateway.ChargeRequest{
		AmountMinor: 12345, Currency: "ZAR", ReturnURL: "https://shop.test/checkout/return",
		Metadata: map[string]string{"session_id": "s1", "transaction_id": "t1"}, IdempotencyKey: key,
	})
}

func TestCapabilities(t *testing.T) {
	d, _ := setup(t)
	assert.False(t, d.SupportsSeparateAuthCapture(), "Yoco is Variant B")
	assert.True(t, d.HostedPayment())
	assert.Equal(t, []string{"ZAR"}, d.SupportedCurrencies())

	ctx, creds := context.Background(), gateway.Credentials{APIKey: apiKey}
	_, err := d.Authorize(ctx, creds, gateway.AuthorizeRequest{})
	require.ErrorIs(t, err, gateway.ErrUnsupported)
	_, err = d.Get(ctx, creds, "ch_1")
	require.ErrorIs(t, err, gateway.ErrUnsupported)
	_, err = d.Void(ctx, creds, "ch_1", "v1")
	require.ErrorIs(t, err, gateway.ErrUnsupported)
}

func TestChargeIsAHostedCheckout(t *testing.T) {
	d, fake := setup(t)
	p, err := charge(t, d, "k1")
	require.NoError(t, err)
	assert.Equal(t, gateway.StatusPending, p.Status)
	require.NotEmpty(t, p.Reference)
	assert.Equal(t, "redirect_to_url", p.NextAction["type"])
	redirect, _ := p.NextAction["redirect_to_url"].(map[string]any)
	assert.Equal(t, fake.URL+"/pay/"+p.Reference, redirect["url"])
	assert.Equal(t, "https://shop.test/checkout/return", redirect["return_url"])

	c, ok := fake.Checkout(p.Reference)
	require.True(t, ok)
	assert.EqualValues(t, 12345, c.Amount)
	assert.Equal(t, "s1", c.Metadata["session_id"])
	require.NotNil(t, c.ExternalID)
	assert.Equal(t, "t1", *c.ExternalID)

	again, err := charge(t, d, "k1")
	require.NoError(t, err)
	assert.Equal(t, p.Reference, again.Reference, "Yoco idempotency returns the same checkout")
	assert.Equal(t, 2, fake.Calls("POST /api/checkouts"))

	_, err = d.Charge(context.Background(), gateway.Credentials{APIKey: apiKey}, gateway.ChargeRequest{
		AmountMinor: 100, Currency: "ZAR", IdempotencyKey: "k2",
	})
	var de *gateway.DeclineError
	require.ErrorAs(t, err, &de, "below R2 is refused")
	assert.Equal(t, "invalid_input", de.Code)

	_, err = d.Charge(context.Background(), gateway.Credentials{APIKey: apiKey}, gateway.ChargeRequest{
		AmountMinor: 1000, Currency: "ZAR", PaymentToken: "pm_card_visa", IdempotencyKey: "k3",
	})
	require.ErrorAs(t, err, &de, "Yoco takes no payment token")
}

func TestOutagesAndBadKeys(t *testing.T) {
	d, fake := setup(t)
	fake.FailNext(1)
	_, err := charge(t, d, "k1")
	assert.True(t, ycerrors.HasCode(err, ycerrors.CodeGatewayUnavailable), "5xx is retryable: %v", err)

	_, err = d.Charge(context.Background(), gateway.Credentials{APIKey: "sk_live_wrong"}, gateway.ChargeRequest{
		AmountMinor: 1000, Currency: "ZAR", IdempotencyKey: "k2",
	})
	var de *gateway.DeclineError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "unauthorized", de.Code)
	require.ErrorAs(t, d.Ping(context.Background(), gateway.Credentials{APIKey: "nope"}), &de)
	require.NoError(t, d.Ping(context.Background(), gateway.Credentials{APIKey: apiKey}))
}

func TestRefund(t *testing.T) {
	d, fake := setup(t)
	ctx, creds := context.Background(), gateway.Credentials{APIKey: apiKey}
	p, err := charge(t, d, "k1")
	require.NoError(t, err)

	_, err = d.Refund(ctx, creds, p.Reference, 0, "r0")
	var de *gateway.DeclineError
	require.ErrorAs(t, err, &de, "an unpaid checkout has nothing to refund")
	assert.Equal(t, "checkout_not_paid", de.Code)

	_, _, err = fake.Pay(t.Context(), p.Reference, true)
	require.NoError(t, err)
	r, err := d.Refund(ctx, creds, p.Reference, 345, "r1")
	require.NoError(t, err)
	assert.Equal(t, gateway.RefundCompleted, r.Status)
	assert.NotEmpty(t, r.Reference)
	again, err := d.Refund(ctx, creds, p.Reference, 345, "r1")
	require.NoError(t, err)
	assert.Equal(t, r.Reference, again.Reference, "a repeated key is the same refund")

	fake.RefundsPending = true
	r, err = d.Refund(ctx, creds, p.Reference, 0, "r2")
	require.NoError(t, err)
	assert.Equal(t, gateway.RefundPending, r.Status)
	_, err = d.Refund(ctx, creds, p.Reference, 1, "r3")
	require.ErrorAs(t, err, &de, "everything is refunded or pending")
	assert.Equal(t, "amount_too_large", de.Code)
}

// receiver records webhooks delivered to it.
type receiver struct {
	mu     sync.Mutex
	got    []http.Header
	bodies [][]byte
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.got, r.bodies = append(r.got, req.Header.Clone()), append(r.bodies, body)
	r.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func TestWebhooks(t *testing.T) {
	d, fake := setup(t)
	rcv := &receiver{}
	endpoint := httptest.NewServer(rcv)
	t.Cleanup(endpoint.Close)
	hook := fake.Register(apiKey, "yc", endpoint.URL+"/webhooks/yoco")
	require.Contains(t, hook.Secret, "whsec_")

	p, err := charge(t, d, "k1")
	require.NoError(t, err)
	ev, deliveries, err := fake.Pay(t.Context(), p.Reference, true)
	require.NoError(t, err)
	require.Len(t, deliveries, 1)
	assert.Equal(t, http.StatusOK, deliveries[0].Status)

	now := time.Now()
	got, err := d.ParseWebhook(hook.Secret, rcv.got[0], rcv.bodies[0], now)
	require.NoError(t, err)
	assert.Equal(t, ev.ID, got.ID)
	assert.Equal(t, "payment.succeeded", got.Type)
	assert.Equal(t, gateway.WebhookPayment, got.Kind)
	assert.Equal(t, p.Reference, got.PaymentReference, "matched by metadata.checkoutId")
	assert.Equal(t, gateway.StatusCaptured, got.Payment.Status)
	assert.EqualValues(t, 12345, got.Payment.AmountCaptured)

	redelivered, ok := fake.Redeliver(t.Context(), ev.ID)
	require.True(t, ok)
	require.Len(t, redelivered, 1)
	again, err := d.ParseWebhook(hook.Secret, rcv.got[1], rcv.bodies[1], now)
	require.NoError(t, err)
	assert.Equal(t, got.ID, again.ID, "a redelivery keeps the event ID")

	_, err = d.ParseWebhook("whsec_"+"d3Jvbmctc2VjcmV0", rcv.got[0], rcv.bodies[0], now)
	require.ErrorIs(t, err, gateway.ErrSignature, "another secret")
	_, err = d.ParseWebhook(hook.Secret, rcv.got[0], append([]byte(" "), rcv.bodies[0]...), now)
	require.ErrorIs(t, err, gateway.ErrSignature, "a changed body")
	_, err = d.ParseWebhook(hook.Secret, rcv.got[0], rcv.bodies[0], now.Add(yoco.Tolerance+time.Minute))
	require.ErrorIs(t, err, gateway.ErrSignature, "too old")
	_, err = d.ParseWebhook("", rcv.got[0], rcv.bodies[0], now)
	require.ErrorIs(t, err, gateway.ErrSignature, "no secret configured")

	failed, err := charge(t, d, "k2")
	require.NoError(t, err)
	_, _, err = fake.Pay(t.Context(), failed.Reference, false)
	require.NoError(t, err)
	got, err = d.ParseWebhook(hook.Secret, rcv.got[2], rcv.bodies[2], now)
	require.NoError(t, err)
	assert.Equal(t, gateway.StatusFailed, got.Payment.Status)
	assert.Equal(t, "payment_failed", got.Payment.FailureReason)
}

func TestRefundWebhook(t *testing.T) {
	d, fake := setup(t)
	rcv := &receiver{}
	endpoint := httptest.NewServer(rcv)
	t.Cleanup(endpoint.Close)
	hook := fake.Register(apiKey, "yc", endpoint.URL)
	p, err := charge(t, d, "k1")
	require.NoError(t, err)
	_, _, err = fake.Pay(t.Context(), p.Reference, true)
	require.NoError(t, err)
	fake.RefundsPending = true
	r, err := d.Refund(context.Background(), gateway.Credentials{APIKey: apiKey}, p.Reference, 0, "r1")
	require.NoError(t, err)
	require.Len(t, fake.SettleRefund(t.Context(), r.Reference, true), 1)

	got, err := d.ParseWebhook(hook.Secret, rcv.got[1], rcv.bodies[1], time.Now())
	require.NoError(t, err)
	assert.Equal(t, gateway.WebhookRefund, got.Kind)
	assert.Equal(t, r.Reference, got.Refund.Reference)
	assert.Equal(t, gateway.RefundCompleted, got.Refund.Status)
	assert.Equal(t, p.Reference, got.PaymentReference)
	assert.EqualValues(t, 12345, got.RefundAmount)
}

// TestStandardWebhooksVector checks the signature against the Standard
// Webhooks specification's published example.
func TestStandardWebhooksVector(t *testing.T) {
	secret := "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"
	id, ts := "msg_p5jXN8AQM9LWM0D4loKWxJek", int64(1614265330)
	body := []byte(`{"test": 2432232314}`)
	sig, err := yoco.Sign(secret, id, time.Unix(ts, 0), body)
	require.NoError(t, err)
	assert.Equal(t, "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE=", sig)

	h := http.Header{}
	h.Set(yoco.IDHeader, id)
	h.Set(yoco.TimestampHeader, strconv.FormatInt(ts, 10))
	h.Set(yoco.SignatureHeader, "v1,bm90LXRoaXMtb25l "+sig)
	_, err = (&yoco.Driver{}).ParseWebhook(secret, h, body, time.Unix(ts, 0))
	require.Error(t, err, "a valid signature, but not a Yoco event")
	require.NotErrorIs(t, err, gateway.ErrSignature, "any v1 entry may match")
}
