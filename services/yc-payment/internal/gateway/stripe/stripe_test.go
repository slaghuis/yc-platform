package stripe_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"

	"github.com/slaghuis/yc-platform/services/yc-payment/internal/gateway"
	"github.com/slaghuis/yc-platform/services/yc-payment/internal/gateway/stripe"
	"github.com/slaghuis/yc-platform/services/yc-payment/internal/gateway/stripe/stripetest"
)

var creds = gateway.Credentials{APIKey: "sk_test_123", WebhookSecret: "whsec_test"}

func setup(t *testing.T) (*stripe.Driver, *stripetest.Server) {
	t.Helper()
	fake := stripetest.NewServer(t)
	return stripe.New(fake.URL, http.DefaultClient), fake
}

func authorize(t *testing.T, d *stripe.Driver, card, key string) (gateway.Payment, error) {
	t.Helper()
	return d.Authorize(context.Background(), creds, gateway.AuthorizeRequest{
		AmountMinor: 12345, Currency: "ZAR", PaymentToken: card,
		Metadata: map[string]string{"session_id": "s1"}, IdempotencyKey: key,
	})
}

func TestAuthorizeCaptureRefund(t *testing.T) {
	d, fake := setup(t)
	ctx := context.Background()

	p, err := authorize(t, d, stripetest.CardVisa, "k1")
	require.NoError(t, err)
	assert.Equal(t, gateway.StatusAuthorized, p.Status)
	pi, _ := fake.Intent(p.Reference)
	assert.Equal(t, "zar", pi.Currency)
	assert.Equal(t, "manual", pi.CaptureMethod)
	assert.Equal(t, "s1", pi.Metadata["session_id"])

	again, err := authorize(t, d, stripetest.CardVisa, "k1")
	require.NoError(t, err)
	assert.Equal(t, p.Reference, again.Reference, "Stripe idempotency returns the same intent")

	p, err = d.Capture(ctx, creds, p.Reference, 0, "c1")
	require.NoError(t, err)
	assert.Equal(t, gateway.StatusCaptured, p.Status)
	assert.EqualValues(t, 12345, p.AmountCaptured)

	_, err = d.Void(ctx, creds, p.Reference, "v1")
	var de *gateway.DeclineError
	require.ErrorAs(t, err, &de, "a captured intent cannot be cancelled")
	assert.Equal(t, "payment_intent_unexpected_state", de.Code)
	require.NotNil(t, de.Payment)
	assert.Equal(t, gateway.StatusCaptured, de.Payment.Status)

	r, err := d.Refund(ctx, creds, p.Reference, 345, "r1")
	require.NoError(t, err)
	assert.Equal(t, gateway.RefundCompleted, r.Status)
	_, err = d.Refund(ctx, creds, p.Reference, 20000, "r2")
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "amount_too_large", de.Code)
}

func TestDeclinedAnd3DS(t *testing.T) {
	d, fake := setup(t)
	ctx := context.Background()

	_, err := authorize(t, d, stripetest.CardDeclined, "k1")
	var de *gateway.DeclineError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "generic_decline", de.Code)
	require.NotNil(t, de.Payment)
	assert.Equal(t, gateway.StatusFailed, de.Payment.Status)
	assert.Equal(t, "generic_decline", de.Payment.FailureReason)

	p, err := authorize(t, d, stripetest.Card3DS, "k2")
	require.NoError(t, err)
	assert.Equal(t, gateway.StatusPending, p.Status)
	assert.Equal(t, "use_stripe_sdk", p.NextAction["type"])

	fake.CompleteAction(p.Reference, true)
	p, err = d.Get(ctx, creds, p.Reference)
	require.NoError(t, err)
	assert.Equal(t, gateway.StatusAuthorized, p.Status)

	p, err = d.Void(ctx, creds, p.Reference, "v1")
	require.NoError(t, err)
	assert.Equal(t, gateway.StatusVoided, p.Status)

	p, err = authorize(t, d, stripetest.Card3DS, "k3")
	require.NoError(t, err)
	fake.CompleteAction(p.Reference, false)
	p, err = d.Get(ctx, creds, p.Reference)
	require.NoError(t, err)
	assert.Equal(t, gateway.StatusFailed, p.Status)
	assert.Equal(t, "payment_intent_authentication_failure", p.FailureReason)
}

func TestOutagesAndBadKeys(t *testing.T) {
	d, fake := setup(t)
	ctx := context.Background()

	fake.FailNext(1)
	_, err := authorize(t, d, stripetest.CardVisa, "k1")
	assert.True(t, ycerrors.HasCode(err, ycerrors.CodeGatewayUnavailable), err)
	var de *gateway.DeclineError
	assert.False(t, errors.As(err, &de))

	require.NoError(t, d.Ping(ctx, creds))
	err = d.Ping(ctx, gateway.Credentials{APIKey: "sk_live_nope"})
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "api_key_invalid", de.Code)

	_, err = stripe.New("http://127.0.0.1:1", http.DefaultClient).Get(ctx, creds, "pi_x")
	assert.True(t, ycerrors.HasCode(err, ycerrors.CodeGatewayUnavailable), err)
}

func TestWebhook(t *testing.T) {
	d, fake := setup(t)
	p, err := authorize(t, d, stripetest.CardVisa, "k1")
	require.NoError(t, err)

	body, sig := fake.Webhook("whsec_test", "payment_intent.amount_capturable_updated", p.Reference)
	h := http.Header{}
	h.Set(stripe.SignatureHeader, sig)
	ev, err := d.ParseWebhook("whsec_test", h, body, time.Now())
	require.NoError(t, err)
	assert.Equal(t, gateway.WebhookPayment, ev.Kind)
	assert.Equal(t, p.Reference, ev.PaymentReference)
	assert.Equal(t, gateway.StatusAuthorized, ev.Payment.Status)

	_, err = d.ParseWebhook("whsec_other", h, body, time.Now())
	require.ErrorIs(t, err, gateway.ErrSignature)
	_, err = d.ParseWebhook("whsec_test", h, append(body, ' '), time.Now())
	require.ErrorIs(t, err, gateway.ErrSignature, "tampered body")
	_, err = d.ParseWebhook("whsec_test", h, body, time.Now().Add(10*time.Minute))
	require.ErrorIs(t, err, gateway.ErrSignature, "replayed too late")
	_, err = d.ParseWebhook("whsec_test", http.Header{}, body, time.Now())
	require.ErrorIs(t, err, gateway.ErrSignature)

	p, err = d.Capture(context.Background(), creds, p.Reference, 0, "")
	require.NoError(t, err)
	r, err := d.Refund(context.Background(), creds, p.Reference, 0, "")
	require.NoError(t, err)
	body, sig = fake.Webhook("whsec_test", "refund.updated", r.Reference)
	h.Set(stripe.SignatureHeader, sig)
	ev, err = d.ParseWebhook("whsec_test", h, body, time.Now())
	require.NoError(t, err)
	assert.Equal(t, gateway.WebhookRefund, ev.Kind)
	assert.Equal(t, r.Reference, ev.Refund.Reference)
	assert.Equal(t, p.Reference, ev.PaymentReference)

	body, sig = fake.Webhook("whsec_test", "customer.created", "cus_1")
	h.Set(stripe.SignatureHeader, sig)
	ev, err = d.ParseWebhook("whsec_test", h, body, time.Now())
	require.NoError(t, err)
	assert.Equal(t, gateway.WebhookOther, ev.Kind)
}
