package domain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/slaghuis/yc-platform/services/yc-checkout/internal/domain"
)

func session(userType domain.UserType, physical bool) domain.Session {
	s := domain.Session{ID: "s1", UserType: userType, State: domain.StatePricing, ItemsTotalMinor: 1000, TotalMinor: 1000}
	if physical {
		s.RequiresShipping = true
		s.ShippingOptions = []domain.ShippingOption{{ID: "standard", Name: "Standard", AmountMinor: 500}}
		s.SelectShipping("standard")
	}
	return s
}

func fields(vs []domain.Violation) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.Field
	}
	return out
}

func TestValidateConfirm(t *testing.T) {
	guest := &domain.GuestContact{Email: "guest@example.com", PhoneE164: "+27831234567"}

	assert.Empty(t, session(domain.UserVisitor, false).ValidateConfirm(domain.ConfirmInput{GuestContact: guest, PaymentToken: "pm"}),
		"digital-only guests need no address (ADR-010)")
	assert.Equal(t, []string{"guest_contact"},
		fields(session(domain.UserVisitor, false).ValidateConfirm(domain.ConfirmInput{PaymentToken: "pm"})))
	assert.Equal(t, []string{"guest_contact.email", "guest_contact.phone_e164"},
		fields(session(domain.UserVisitor, false).ValidateConfirm(domain.ConfirmInput{
			GuestContact: &domain.GuestContact{Email: "Guest <g@example.com>", PhoneE164: "0831234567"}, PaymentToken: "pm",
		})))
	assert.Equal(t, []string{"delivery_address", "payment_gateway_token"},
		fields(session(domain.UserCustomer, true).ValidateConfirm(domain.ConfirmInput{})))
	assert.Equal(t, []string{"shipping_option_id"},
		fields(session(domain.UserCustomer, true).ValidateConfirm(domain.ConfirmInput{
			DeliveryAddress: map[string]any{"line1": "x"}, ShippingOptionID: "express", PaymentToken: "pm",
		})))
}

func TestShipping(t *testing.T) {
	s := session(domain.UserCustomer, true)
	assert.Equal(t, int64(1500), s.TotalMinor, "flat fee added, untaxed")
	assert.False(t, s.SelectShipping("express"))
	o, ok := s.SelectedShipping()
	require.True(t, ok)
	assert.Equal(t, "Standard", o.Name)
}

func TestSteps(t *testing.T) {
	now := time.Now()
	s := session(domain.UserCustomer, false)
	s.Confirm(domain.ConfirmInput{PaymentToken: "pm", PaymentGateway: "stripe"}, "k", now)
	assert.Equal(t, domain.StateAuthorizing, s.State, "nothing tracked: RESERVING is skipped")
	s.HasTrackedItems = true
	assert.Equal(t, domain.StateReserving, s.FirstStep())

	assert.Equal(t, []domain.State{
		domain.StateReserving, domain.StateAuthorizing, domain.StateOrderCreate,
		domain.StateCapturing, domain.StateConfirming,
	}, walk(s), "Variant A")

	assert.True(t, domain.StateCapturing.Compensable())
	assert.False(t, domain.StateConfirming.Compensable(), "capture succeeded: forward only")
	s.State = domain.StateConfirming
	require.Error(t, s.Compensate(domain.ReasonSessionExpired, nil, domain.StateExpired, now))

	s.State = domain.StateOrderCreate
	require.NoError(t, s.Compensate(domain.ReasonStepFailed, nil, domain.StateFailed, now))
	assert.Equal(t, domain.StateCompensating, s.State)
	assert.True(t, s.Reached(domain.StateAuthorizing))
	assert.True(t, s.Reached(domain.StateOrderCreate))
	assert.False(t, s.Reached(domain.StateCapturing))

	s.Undo(domain.UndoPayment, "voided", now)
	s.Undo(domain.UndoPayment, "voided", now)
	assert.Len(t, s.CompensationLog, 1)
	s.Finish(now)
	assert.Equal(t, domain.StateFailed, s.State)
	assert.Empty(t, s.PaymentToken)
}

func walk(s domain.Session) []domain.State {
	var got []domain.State
	for s.State = domain.StateReserving; s.State != domain.StateCompleted; s.State = s.Next() {
		got = append(got, s.State)
	}
	return got
}

func TestVariantB(t *testing.T) {
	now := time.Now()
	s := session(domain.UserCustomer, false)
	s.Route(domain.Routing{Gateway: "yoco", Variant: domain.VariantCharge, Hosted: true})
	assert.Equal(t, domain.StateCharging, s.PaymentState())
	assert.Equal(t, "charge", s.PaymentCall())

	assert.Equal(t, []string{"payment_gateway_token", "return_url"},
		fields(s.ValidateConfirm(domain.ConfirmInput{PaymentToken: "pm"})), "hosted: no token, a return URL")
	assert.Empty(t, s.ValidateConfirm(domain.ConfirmInput{ReturnURL: "https://shop.example/return"}))

	s.Confirm(domain.ConfirmInput{ReturnURL: "https://shop.example/return"}, "k", now)
	assert.Equal(t, domain.StateOrderCreate, s.State, "nothing tracked: the order comes first")
	s.HasTrackedItems = true
	assert.Equal(t, []domain.State{
		domain.StateReserving, domain.StateOrderCreate, domain.StateCharging, domain.StateConfirming,
	}, walk(s))

	assert.True(t, domain.StateCharging.Compensable(), "a charge awaiting the customer is compensated")
	assert.True(t, domain.StateCharging.Driven())
	s.State = domain.StateOrderCreate
	require.NoError(t, s.Compensate(domain.ReasonStepFailed, nil, domain.StateFailed, now))
	assert.True(t, s.Reached(domain.StateOrderCreate))
	assert.False(t, s.Reached(domain.StateCharging), "an order failure moves no money (ADR-019 rule 3)")
	assert.False(t, s.Reached(domain.StateAuthorizing))
}

func TestRetryLater(t *testing.T) {
	now := time.Now()
	var s domain.Session
	delays := make([]time.Duration, 0, 5)
	for range 5 {
		s.RetryLater(now, time.Second, 5*time.Second)
		delays = append(delays, s.NextAttemptAt.Sub(now))
	}
	assert.Equal(t, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second}, delays)
}
