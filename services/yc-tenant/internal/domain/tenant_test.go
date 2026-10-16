package domain_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/slaghuis/yc-platform/services/yc-tenant/internal/domain"
)

func TestCreateInputValidate(t *testing.T) {
	in, err := (domain.CreateInput{
		TenantID: "shop-abc", Name: " Shop ABC ",
		Hostnames:     []string{"Shop-ABC.localhost", "www.shop-abc.localhost"},
		StoreCurrency: "ZAR", DefaultJurisdiction: "ZA", IsTaxInclusive: true,
		ContactEmail: "ops@shop-abc.test", PrimaryAdminEmail: " admin@shop-abc.test ",
	}).Validate()
	require.NoError(t, err)
	assert.Equal(t, "Shop ABC", in.Name)
	assert.Equal(t, []string{"shop-abc.localhost", "www.shop-abc.localhost"}, in.Hostnames)
	assert.Equal(t, "admin@shop-abc.test", in.PrimaryAdminEmail)
}

func TestCreateInputRejects(t *testing.T) {
	_, err := (domain.CreateInput{
		TenantID: "_platform", Name: "x", Hostnames: []string{"a.test"},
		StoreCurrency: "ZAR", DefaultJurisdiction: "ZA", ContactEmail: "a@b.c",
	}).Validate()
	assert.Error(t, err)

	_, err = (domain.CreateInput{
		TenantID: "shop-a", Name: "x", Hostnames: []string{"admin.localhost"},
		StoreCurrency: "zar", DefaultJurisdiction: "ZA", ContactEmail: "a@b.c",
	}).Validate()
	assert.Error(t, err)
}

func TestHostnamesInputOrdersPrimaryFirst(t *testing.T) {
	hs, err := (domain.HostnamesInput{
		Hostnames: []string{"b.test", "a.test", "c.test"}, Primary: "a.test",
	}).Validate()
	require.NoError(t, err)
	require.Len(t, hs, 3)
	assert.True(t, hs[0].IsPrimary)
	assert.Equal(t, "a.test", hs[0].Name)
	assert.Equal(t, []string{"a.test", "b.test", "c.test"}, domain.HostnameNames(hs))
}

func TestValidateReason(t *testing.T) {
	_, err := domain.ValidateReason("  ")
	assert.Error(t, err)
	r, err := domain.ValidateReason(" non-payment ")
	require.NoError(t, err)
	assert.Equal(t, "non-payment", r)
}
