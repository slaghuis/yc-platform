package domain_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/domain"
)

func TestValidateBilling(t *testing.T) {
	ok := domain.BillingAddress{
		CompanyName: "Acme", VATNumber: "4123456789",
		StreetAndNumber: "1 Long St", Suburb: "City Bowl", TownCity: "Cape Town", PostalCode: "8001",
	}
	require.NoError(t, domain.ValidateBilling(ok))

	n := domain.NormaliseBilling(domain.BillingAddress{
		VATNumber: " 4123456789 ", StreetAndNumber: " 1 Long St ", TownCity: "Cape Town", PostalCode: "8001",
	})
	assert.Equal(t, "ZA", n.CountryCode)
	assert.Equal(t, "4123456789", n.VATNumber)

	err := domain.ValidateBilling(domain.BillingAddress{TownCity: "X", PostalCode: "1"})
	require.Error(t, err)
	assert.True(t, ycerrors.HasCode(err, ycerrors.CodeValidationFailed))

	err = domain.ValidateBilling(domain.BillingAddress{
		StreetAndNumber: "1", TownCity: "X", PostalCode: "1", VATNumber: "123",
	})
	require.Error(t, err)
}
