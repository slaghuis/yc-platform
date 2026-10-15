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

func order() domain.Order {
	return domain.Order{
		CheckoutSessionID: uuid.NewString(), UserID: "u1", UserType: domain.UserCustomer,
		RequiresShipping: true, DeliveryAddress: map[string]any{"city": "Cape Town"},
		Currency: "ZAR", PricingSnapshot: json.RawMessage(`{}`),
		Items: []domain.Item{
			{SKUID: uuid.NewString(), NameSnapshot: "Mug", Quantity: 2, UnitPriceMinor: 500, LineTotalMinor: 1000},
			{SKUID: uuid.NewString(), NameSnapshot: "E-book", Quantity: 1, UnitPriceMinor: 300, LineTotalMinor: 300, IsDigital: true},
		},
		ItemsTotalMinor: 1300, ShippingMinor: 100, TotalMinor: 1400,
	}
}

func fields(t *testing.T, err error) []string {
	t.Helper()
	e, ok := ycerrors.As(err)
	require.True(t, ok, err)
	out := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		out = append(out, f.Field)
	}
	return out
}

func TestValidate(t *testing.T) {
	require.NoError(t, order().Validate())

	o := order()
	o.Items[0].LineTotalMinor = -1
	o.ItemsTotalMinor = 299
	assert.Contains(t, fields(t, o.Validate()), "items[0].line_total_minor")

	o = order()
	o.ItemsTotalMinor = 1
	assert.Contains(t, fields(t, o.Validate()), "items_total")

	o = order()
	o.RequiresShipping = false
	o.DeliveryAddress = nil
	assert.Contains(t, fields(t, o.Validate()), "requires_shipping", "a physical item needs shipping")

	o = order()
	o.UserType = domain.UserVisitor
	assert.Contains(t, fields(t, o.Validate()), "guest_contact")
	o.GuestContact = &domain.GuestContact{Email: "not-an-email", PhoneE164: "0821234567"}
	assert.Subset(t, fields(t, o.Validate()), []string{"guest_contact.email", "guest_contact.phone_e164"})
	o.GuestContact = &domain.GuestContact{Email: "Guest@Example.com", PhoneE164: "+27821234567"}
	require.NoError(t, o.Validate())
	assert.Equal(t, "guest@example.com", o.SourceEmail())
}

func TestTransition(t *testing.T) {
	now := time.Now()
	o := domain.Order{Status: domain.StatusPendingPayment}
	require.NoError(t, o.Transition(domain.StatusReceived, now))
	assert.Equal(t, now, *o.PlacedAt)
	require.NoError(t, o.Transition(domain.StatusReceived, now), "confirming again")
	assert.True(t, ycerrors.HasCode(o.Transition(domain.StatusAbandoned, now), ycerrors.CodeInvalidStateTransition))
	require.NoError(t, o.Transition(domain.StatusCancelled, now))

	o = domain.Order{Status: domain.StatusPendingPayment}
	require.NoError(t, o.Transition(domain.StatusAbandoned, now))
	assert.Error(t, o.Transition(domain.StatusReceived, now))
	assert.Error(t, o.Transition(domain.StatusCancelled, now))
}

func TestOrderNumber(t *testing.T) {
	assert.Equal(t, "DEMO-SHOP-000042", domain.OrderNumber("demo-shop", 42))
	assert.Equal(t, "SHOP-A-1234567", domain.OrderNumber("shop-a", 1234567))
}
