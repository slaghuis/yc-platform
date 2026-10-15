package domain_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"

	"github.com/slaghuis/yc-platform/services/yc-orders/internal/domain"
)

func TestFulfilmentTransitions(t *testing.T) {
	now := time.Now()
	o := order()
	o.Status = domain.StatusReceived
	require.NoError(t, o.Transition(domain.StatusPackaged, now))
	assert.Equal(t, now, *o.Shipment.PackagedAt)
	require.NoError(t, o.Transition(domain.StatusDeliveryException, now))
	require.NoError(t, o.Transition(domain.StatusShipped, now), "back on its way")
	assert.True(t, ycerrors.HasCode(o.Transition(domain.StatusCancelled, now), ycerrors.CodeInvalidStateTransition),
		"a shipped order is not cancelled")
	require.NoError(t, o.Transition(domain.StatusDelivered, now))
	assert.False(t, o.Status.CanBecome(domain.StatusShipped), "a late in-transit event changes nothing")
	require.NoError(t, o.Transition(domain.StatusReturned, now))

	o.Status = domain.StatusPackaged
	require.NoError(t, o.Transition(domain.StatusDelivered, now), "delivered may arrive before in transit")
	o.Status = domain.StatusPackaged
	require.NoError(t, o.Transition(domain.StatusCancelled, now), "a packaged order can still be cancelled")

	digital := order()
	digital.Status, digital.RequiresShipping = domain.StatusReceived, false
	assert.Error(t, digital.Transition(domain.StatusPackaged, now), "a digital order is not shipped")

	assert.True(t, domain.StatusDeliveryException.Placed())
	assert.False(t, domain.StatusAbandoned.Placed())
}

func TestShippingChoice(t *testing.T) {
	o := order()
	rate := uuid.NewString()
	o.ShippingSnapshot = json.RawMessage(`{"id":"express","rate_id":"` + rate + `","amount_minor":100}`)
	assert.Equal(t, domain.ShippingChoice{Method: "express", RateID: rate}, o.ShippingChoice())
	o.ShippingSnapshot = json.RawMessage(`{"method":"standard","rate_id":"nope"}`)
	assert.Equal(t, domain.ShippingChoice{Method: "standard"}, o.ShippingChoice())
	o.ShippingSnapshot = nil
	assert.Equal(t, "standard", o.ShippingChoice().Method)
}

func delivered(at time.Time) domain.Order {
	o := order()
	o.Status, o.Shipment.DeliveredAt, o.PaymentTransactionID = domain.StatusDelivered, &at, uuid.NewString()
	return o
}

func TestReturns(t *testing.T) {
	now := time.Now()
	o := delivered(now.Add(-24 * time.Hour))
	mug, ebook := o.Items[0].SKUID, o.Items[1].SKUID
	require.NoError(t, o.CheckReturnable(now))
	assert.Error(t, delivered(now.Add(-31*24*time.Hour)).CheckReturnable(now), "the window has closed")
	shipped := order()
	shipped.Status = domain.StatusShipped
	assert.Error(t, shipped.CheckReturnable(now), "not delivered yet")

	assert.Equal(t, []string{"items[0].sku_id"}, fields(t, o.ValidateReturn([]domain.ReturnItem{{SKUID: ebook, Quantity: 1}}, "", nil)),
		"digital items are not returned")
	assert.Equal(t, []string{"items[0].quantity"}, fields(t, o.ValidateReturn([]domain.ReturnItem{{SKUID: mug, Quantity: 3}}, "", nil)))
	assert.Equal(t, []string{"items[0].sku_id"}, fields(t, o.ValidateReturn([]domain.ReturnItem{{SKUID: uuid.NewString(), Quantity: 1}}, "", nil)))
	assert.Equal(t, []string{"items[1].sku_id"}, fields(t, o.ValidateReturn([]domain.ReturnItem{{SKUID: mug, Quantity: 1}, {SKUID: mug, Quantity: 1}}, "", nil)))

	// One mug of two is returned and accepted; the other later.
	first := domain.Return{ID: "r1", Status: domain.ReturnRequested, Items: []domain.ReturnItem{{SKUID: mug, Quantity: 1}}}
	require.NoError(t, o.ValidateReturn(first.Items, "chipped", nil))
	assert.Equal(t, int64(500), o.ReturnRefund(first, false, nil))
	assert.Equal(t, int64(600), o.ReturnRefund(first, true, nil), "with shipping")
	first.Status, first.RefundShipping = domain.ReturnAccepted, true
	earlier := make([]domain.Return, 0, 2)
	earlier = append(earlier, first)
	assert.False(t, o.FullyReturned(earlier))
	assert.Error(t, o.ValidateReturn([]domain.ReturnItem{{SKUID: mug, Quantity: 2}}, "", earlier), "one is left")

	second := domain.Return{ID: "r2", Status: domain.ReturnRequested, Items: []domain.ReturnItem{{SKUID: mug, Quantity: 1}}}
	assert.Equal(t, int64(500), o.ReturnRefund(second, true, earlier), "shipping is refunded once")
	second.Status = domain.ReturnAccepted
	assert.True(t, o.FullyReturned(append(earlier, second)), "every physical line is back")

	o.RefundedMinor = 1200
	assert.Equal(t, int64(200), o.ReturnRefund(second, false, earlier), "capped at what is left")
}

func TestReturnRefundRoundsCumulatively(t *testing.T) {
	o := delivered(time.Now())
	o.Items[0].Quantity, o.Items[0].LineTotalMinor = 3, 1000
	mug := o.Items[0].SKUID
	earlier := make([]domain.Return, 0, 3)
	var total int64
	for range 3 {
		r := domain.Return{ID: uuid.NewString(), Items: []domain.ReturnItem{{SKUID: mug, Quantity: 1}}}
		total += o.ReturnRefund(r, false, earlier)
		r.Status = domain.ReturnAccepted
		earlier = append(earlier, r)
	}
	assert.Equal(t, int64(1000), total, "three thirds make the whole line")
}

func TestValidateRefund(t *testing.T) {
	o := delivered(time.Now())
	require.NoError(t, o.ValidateRefund(1400, "damaged"))
	assert.Equal(t, []string{"amount_minor"}, fields(t, o.ValidateRefund(1401, "too much")))
	assert.Equal(t, []string{"reason"}, fields(t, o.ValidateRefund(1, " ")))
	o.PaymentTransactionID = ""
	assert.True(t, ycerrors.HasCode(o.ValidateRefund(1, "x"), ycerrors.CodeInvalidStateTransition))
}
