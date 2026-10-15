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
