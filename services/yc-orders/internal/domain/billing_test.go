package domain_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	"github.com/slaghuis/yc-platform/services/yc-orders/internal/domain"
)

func TestValidateBilling(t *testing.T) {
	ok := domain.BillingAddress{
		StreetAndNumber: "1 Long St", TownCity: "Cape Town", PostalCode: "8001",
	}
	require.NoError(t, domain.ValidateBilling(ok))
	n := domain.NormaliseBilling(domain.BillingAddress{
		StreetAndNumber: " 1 ", TownCity: " CT ", PostalCode: " 8001 ", VATNumber: " 4123456789 ",
	})
	assert.Equal(t, "ZA", n.CountryCode)
	assert.Equal(t, "4123456789", n.VATNumber)

	err := domain.ValidateBilling(domain.BillingAddress{TownCity: "X", PostalCode: "1"})
	require.Error(t, err)
	e, okErr := ycerrors.As(err)
	require.True(t, okErr)
	assert.NotEmpty(t, e.Fields)

	err = domain.ValidateBilling(domain.BillingAddress{
		StreetAndNumber: "1", TownCity: "X", PostalCode: "1", VATNumber: "bad",
	})
	require.Error(t, err)
}

func TestInvoicePDFTitle(t *testing.T) {
	o := domain.Order{
		OrderNumber: "DEMO-000001", Currency: "ZAR", ItemsTotalMinor: 1000, TaxTotalMinor: 150, TotalMinor: 1150,
		Items:         []domain.Item{{NameSnapshot: "Widget", Quantity: 1, LineTotalMinor: 1000}},
		SellerInvoice: &domain.SellerInvoice{Name: "Shop", ContactEmail: "ops@shop.test"},
	}
	pdf := domain.InvoicePDF(o)
	assert.Contains(t, string(pdf), "Invoice")
	assert.NotContains(t, string(pdf), "Tax Invoice")

	o.SellerInvoice.VATNumber = "4123456789"
	pdf = domain.InvoicePDF(o)
	assert.Contains(t, string(pdf), "Tax Invoice")
	assert.True(t, len(pdf) > 100)
	assert.True(t, bytesHasPDFHeader(pdf))
}

func bytesHasPDFHeader(b []byte) bool {
	return len(b) >= 5 && string(b[:5]) == "%PDF-"
}
