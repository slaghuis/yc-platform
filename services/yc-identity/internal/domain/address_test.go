package domain_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/domain"
)

func TestValidateDeliveryAddressInput(t *testing.T) {
	err := domain.ValidateDeliveryAddressInput(domain.DeliveryAddressInput{
		Address: map[string]any{"country_code": "ZA"},
	})
	assert.NoError(t, err)

	err = domain.ValidateDeliveryAddressInput(domain.DeliveryAddressInput{})
	require.Error(t, err)
	assert.True(t, ycerrors.HasCode(err, ycerrors.CodeValidationFailed))

	err = domain.ValidateDeliveryAddressInput(domain.DeliveryAddressInput{
		Label:   strings.Repeat("x", 65),
		Address: map[string]any{"a": 1},
	})
	require.Error(t, err)
}

func TestNormaliseDeliveryAddressInput(t *testing.T) {
	in := domain.NormaliseDeliveryAddressInput(domain.DeliveryAddressInput{Label: "  Home  "})
	assert.Equal(t, "Home", in.Label)
}
