package domain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/slaghuis/yc-platform/services/yc-payment/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-payment/internal/gateway"
)

func TestApplyNeverMovesBackwards(t *testing.T) {
	now := time.Now()
	tx := domain.Transaction{Status: domain.StatusCreated, AmountMinor: 1000}

	assert.True(t, tx.Apply(gateway.Payment{
		Reference: "pi_1", Status: gateway.StatusPending,
		NextAction: map[string]any{"type": "use_stripe_sdk"},
	}, now))
	assert.Equal(t, domain.StatusPending, tx.Status)
	assert.NotNil(t, tx.NextAction)

	assert.True(t, tx.Apply(gateway.Payment{Status: gateway.StatusAuthorized}, now))
	assert.Equal(t, "pi_1", tx.GatewayReference, "kept when the update has none")
	assert.Nil(t, tx.NextAction)

	assert.False(t, tx.Apply(gateway.Payment{Status: gateway.StatusPending}, now), "a late webhook")
	assert.Equal(t, domain.StatusAuthorized, tx.Status)

	assert.True(t, tx.Apply(gateway.Payment{Status: gateway.StatusCaptured}, now))
	assert.EqualValues(t, 1000, tx.CapturedMinor, "the whole amount when the gateway does not say")
	assert.False(t, tx.Apply(gateway.Payment{Status: gateway.StatusVoided}, now), "final states are final")

	assert.EqualValues(t, 1000, tx.Refundable())
	tx.AddRefund(400, now)
	assert.Equal(t, domain.StatusCaptured, tx.Status)
	assert.EqualValues(t, 600, tx.Refundable())
	tx.AddRefund(600, now)
	assert.Equal(t, domain.StatusRefunded, tx.Status)
	assert.Zero(t, tx.Refundable())
}

func TestApplyFailure(t *testing.T) {
	tx := domain.Transaction{Status: domain.StatusPending}
	assert.True(t, tx.Apply(gateway.Payment{Status: gateway.StatusFailed, FailureReason: "card_declined"}, time.Now()))
	assert.Equal(t, domain.StatusFailed, tx.Status)
	assert.Equal(t, "card_declined", tx.FailureReason)
	assert.False(t, tx.Status.Settled() == false)
}

func TestApplyCapturedAfterFailure(t *testing.T) {
	tx := domain.Transaction{Status: domain.StatusFailed, Kind: domain.KindCharge, AmountMinor: 500, FailureReason: "payment_failed"}
	assert.True(t, tx.Apply(gateway.Payment{Status: gateway.StatusCaptured, AmountCaptured: 500}, time.Now()),
		"a customer who paid with another card on the hosted page")
	assert.Equal(t, domain.StatusCaptured, tx.Status)
	assert.EqualValues(t, 500, tx.CapturedMinor)
	assert.False(t, tx.Apply(gateway.Payment{Status: gateway.StatusFailed}, time.Now()), "but not back again")
}

func TestRoute(t *testing.T) {
	supported := func(gw string) ([]string, bool) {
		switch gw {
		case "stripe":
			return nil, true
		case "yoco":
			return []string{"ZAR"}, true
		}
		return nil, false
	}
	stripe := domain.GatewayConfig{Gateway: "stripe", Enabled: true, Priority: 20}
	yoco := domain.GatewayConfig{Gateway: "yoco", Enabled: true, Priority: 10}
	both := []domain.GatewayConfig{stripe, yoco}

	c, ok := domain.Route(both, "ZAR", supported)
	assert.True(t, ok)
	assert.Equal(t, "yoco", c.Gateway, "Yoco has priority for ZAR")
	c, _ = domain.Route(both, "USD", supported)
	assert.Equal(t, "stripe", c.Gateway, "Yoco does not take USD")

	stripeZAR := stripe
	stripeZAR.Priority = 5
	stripeZAR.Currencies = []string{"USD", "EUR"}
	c, _ = domain.Route([]domain.GatewayConfig{stripeZAR, yoco}, "ZAR", supported)
	assert.Equal(t, "yoco", c.Gateway, "the shop routes only USD and EUR to Stripe")
	_, ok = domain.Route([]domain.GatewayConfig{stripeZAR}, "GBP", supported)
	assert.False(t, ok)

	off := yoco
	off.Enabled = false
	c, _ = domain.Route([]domain.GatewayConfig{stripe, off}, "ZAR", supported)
	assert.Equal(t, "stripe", c.Gateway, "a disabled gateway is skipped")
	_, ok = domain.Route([]domain.GatewayConfig{{Gateway: "paypal", Enabled: true}}, "ZAR", supported)
	assert.False(t, ok, "a gateway without a driver is skipped")

	tie := yoco
	tie.Priority = 20
	c, _ = domain.Route([]domain.GatewayConfig{tie, stripe}, "ZAR", supported)
	assert.Equal(t, "stripe", c.Gateway, "ties go by name")
}

func TestValidateRouting(t *testing.T) {
	assert.NoError(t, domain.ValidateRouting(100, nil, nil))
	assert.NoError(t, domain.ValidateRouting(0, []string{"ZAR"}, []string{"ZAR"}))
	assert.Error(t, domain.ValidateRouting(-1, nil, nil))
	assert.Error(t, domain.ValidateRouting(1001, nil, nil))
	assert.Error(t, domain.ValidateRouting(1, []string{"zar"}, nil))
	assert.Error(t, domain.ValidateRouting(1, []string{"USD"}, []string{"ZAR"}), "Yoco takes only ZAR")
	assert.Error(t, domain.ValidateRouting(1, []string{"USD", "USD"}, nil))
}

func TestKeyHint(t *testing.T) {
	assert.Equal(t, "sk_test_…4242", domain.KeyHint("sk_test_51abcdef4242"))
	assert.Equal(t, "…cdef", domain.KeyHint("0123456789abcdef"))
	assert.Equal(t, "…", domain.KeyHint("short"))
	assert.Equal(t, "rk_live_…7890", domain.KeyHint("rk_live_1234567890"))
}

func TestValidateCredentials(t *testing.T) {
	assert.NoError(t, domain.ValidateCredentials(gateway.Credentials{APIKey: "sk_test_x", WebhookSecret: "whsec_y"}, "pk"))
	assert.Error(t, domain.ValidateCredentials(gateway.Credentials{}, ""))
	assert.Error(t, domain.ValidateCredentials(gateway.Credentials{APIKey: "sk test"}, ""))
}
