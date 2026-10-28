package domain_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/slaghuis/yc-platform/services/yc-tenant/internal/domain"
)

func TestShopPatchValidate(t *testing.T) {
	vat := "4123456789"
	got, err := domain.ShopPatch{VATNumber: &vat}.Validate()
	require.NoError(t, err)
	require.NotNil(t, got.VATNumber)
	assert.Equal(t, vat, *got.VATNumber)

	empty := ""
	got, err = domain.ShopPatch{VATNumber: &empty}.Validate()
	require.NoError(t, err)
	require.NotNil(t, got.VATNumber)
	assert.Equal(t, "", *got.VATNumber)

	bad := "nope"
	_, err = domain.ShopPatch{VATNumber: &bad}.Validate()
	require.Error(t, err)

	_, err = domain.ShopPatch{}.Validate()
	require.Error(t, err)

	addr := domain.InvoiceAddress{StreetAndNumber: "1 Main", TownCity: "CT", PostalCode: "8001"}
	got, err = domain.ShopPatch{InvoiceAddress: &addr}.Validate()
	require.NoError(t, err)
	require.NotNil(t, got.InvoiceAddress)
	assert.Equal(t, "ZA", got.InvoiceAddress.CountryCode)

	clear := domain.InvoiceAddress{}
	got, err = domain.ShopPatch{InvoiceAddress: &clear}.Validate()
	require.NoError(t, err)
	require.NotNil(t, got.InvoiceAddress)
	assert.True(t, got.InvoiceAddress.Empty())
}
