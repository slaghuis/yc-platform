package domain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	money "github.com/slaghuis/yc-platform/libs/yc-money"
	"github.com/slaghuis/yc-platform/services/yc-cart/internal/domain"
)

var shopping = domain.CartType{Name: "shopping", Convertible: true, MergeOnLogin: true, MaxItems: 3, MaxQuantity: 10}

func zar(minor int64) money.Money { return money.New(minor, "ZAR") }

func TestTypesValidate(t *testing.T) {
	require.NoError(t, domain.Types{"shopping": shopping}.Validate())
	assert.Error(t, domain.Types{}.Validate())
	assert.Error(t, domain.Types{"Shopping": {Name: "Shopping", MaxItems: 1, MaxQuantity: 1}}.Validate())
	assert.Error(t, domain.Types{"wishlist": {Name: "wishlist", MaxItems: 0, MaxQuantity: 1}}.Validate())

	_, err := domain.Types{"shopping": shopping}.Get("gifts")
	assert.Error(t, err)
}

func TestCheckQuantity(t *testing.T) {
	require.NoError(t, shopping.CheckQuantity(10))
	assert.Error(t, shopping.CheckQuantity(0))
	assert.Error(t, shopping.CheckQuantity(11))
}

func TestMergeItems(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	user := []domain.Item{
		{SKUID: "a", Quantity: 8, UnitPrice: zar(100), AddedAt: t0},
		{SKUID: "b", Quantity: 1, UnitPrice: zar(200), AddedAt: t0},
	}
	visitor := []domain.Item{
		{SKUID: "d", Quantity: 1, UnitPrice: zar(400), AddedAt: t0.Add(2 * time.Hour)},
		{SKUID: "a", Quantity: 5, UnitPrice: zar(150), AddedAt: t0.Add(time.Hour)},
		{SKUID: "c", Quantity: 2, UnitPrice: zar(300), AddedAt: t0.Add(time.Hour)},
	}

	writes, merged := domain.MergeItems(user, visitor, shopping)

	assert.Equal(t, []domain.Item{
		{SKUID: "a", Quantity: 10, UnitPrice: zar(100), AddedAt: t0},
		{SKUID: "c", Quantity: 2, UnitPrice: zar(300), AddedAt: t0.Add(time.Hour)},
	}, writes, "summed and capped with the user's snapshot; oldest new line first; d exceeds max items")
	assert.Equal(t, 2, merged)

	again, _ := domain.MergeItems(user, visitor, shopping)
	assert.Equal(t, writes, again, "deterministic")
}

func TestConvertItems(t *testing.T) {
	lines := []domain.Item{
		{SKUID: "a", Quantity: 2},
		{SKUID: "b", Quantity: 5},
		{SKUID: "c", Quantity: 1},
		{SKUID: "d", Quantity: 1},
	}
	writes, deletes, removed := domain.ConvertItems(lines, map[string]int{"a": 2, "b": 3, "c": 4, "x": 1})
	assert.Equal(t, []domain.Item{{SKUID: "b", Quantity: 2}}, writes, "added during checkout: the difference stays")
	assert.Equal(t, []string{"a", "c"}, deletes)
	assert.Equal(t, map[string]int{"a": 2, "b": 3, "c": 1}, removed, "lowered meanwhile: only what is there")
}

func TestPrice(t *testing.T) {
	c := domain.Cart{Items: []domain.Item{
		{SKUID: "a", Quantity: 2, UnitPrice: zar(100)},
		{SKUID: "b", Quantity: 1, UnitPrice: zar(200)},
		{SKUID: "gone", Quantity: 1, UnitPrice: zar(50)},
	}}
	p := domain.Prices{
		StoreCurrency: "ZAR",
		Units:         map[string]money.Money{"a": zar(100), "b": zar(250)},
		Unpriced:      map[string]string{"gone": "unknown_sku"},
		Display: &domain.DisplayPrices{
			Currency: "USD", Rate: "0.055",
			Units: map[string]money.Money{"a": money.New(6, "USD"), "b": money.New(14, "USD")},
		},
	}

	v, err := domain.Price(c, shopping, p)
	require.NoError(t, err)

	assert.Equal(t, zar(450), v.Subtotal)
	assert.True(t, v.PriceChange)
	assert.False(t, v.Lines[0].PriceChanged)
	assert.Equal(t, zar(200), v.Lines[0].Total)
	assert.True(t, v.Lines[1].PriceChanged)
	assert.Equal(t, zar(250), v.Lines[1].Current)
	assert.False(t, v.Lines[2].Available)
	assert.Equal(t, "unknown_sku", v.Lines[2].Reason)
	require.NotNil(t, v.Display)
	assert.Equal(t, money.New(26, "USD"), v.Display.Subtotal)
	assert.Equal(t, money.New(12, "USD"), v.Lines[0].Display.Total)

	v, err = domain.Price(domain.Cart{}, shopping, domain.Prices{StoreCurrency: "ZAR"})
	require.NoError(t, err)
	assert.Equal(t, zar(0), v.Subtotal)
	assert.Nil(t, v.Display)
}
