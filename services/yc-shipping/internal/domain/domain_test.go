package domain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	"github.com/slaghuis/yc-platform/services/yc-shipping/internal/domain"
)

func addr(country, province string) map[string]any {
	a := map[string]any{"country_code": country, "lines": []any{"1 Main Rd"}}
	if province != "" {
		a["fields"] = map[string]any{"province": province, "city": "Somewhere"}
	}
	return a
}

func TestResolveZone(t *testing.T) {
	zones := []domain.Zone{
		{ID: "gp", Priority: 10, Rules: []domain.Rule{{CountryCode: "ZA", Fields: map[string]string{"province": "Gauteng"}}}},
		{ID: "wc", Priority: 10, Rules: []domain.Rule{{CountryCode: "ZA", Fields: map[string]string{"province": "Western Cape"}}}},
		{ID: "za", Priority: 100, Rules: []domain.Rule{{CountryCode: "ZA"}}},
	}
	for _, tc := range []struct {
		name string
		addr map[string]any
		want string
	}{
		{"province", addr("ZA", "Gauteng"), "gp"},
		{"case and spaces", addr("za", "  western cape "), "wc"},
		{"rest of the country", addr("ZA", "Limpopo"), "za"},
		{"no fields", addr("ZA", ""), "za"},
		{"other country", addr("NA", "Khomas"), ""},
		{"no country", map[string]any{"lines": []any{"x"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			z, ok := domain.ResolveZone(zones, tc.addr)
			assert.Equal(t, tc.want != "", ok)
			assert.Equal(t, tc.want, z.ID)
		})
	}
}

func TestZoneValidate(t *testing.T) {
	z := domain.Zone{Name: " Gauteng ", Priority: 10, Rules: []domain.Rule{{CountryCode: "za", Fields: map[string]string{"province": " Gauteng "}}}}
	z.Normalise()
	require.NoError(t, z.Validate())
	assert.Equal(t, "ZA", z.Rules[0].CountryCode)
	assert.Equal(t, "Gauteng", z.Rules[0].Fields["province"])

	bad := domain.Zone{Name: "", Priority: 2000, Rules: []domain.Rule{{CountryCode: "ZAF", Fields: map[string]string{"bad key": ""}}}}
	err := bad.Validate()
	require.True(t, ycerrors.HasCode(err, ycerrors.CodeValidationFailed))
	e, _ := ycerrors.As(err)
	fields := map[string]bool{}
	for _, f := range e.Fields {
		fields[f.Field] = true
	}
	assert.True(t, fields["name"] && fields["priority"] && fields["match_rules[0].country_code"] && fields["match_rules[0].fields.bad key"])
	assert.Error(t, domain.Zone{Name: "x"}.Validate(), "a zone needs a rule")
}

func days(n int) *int { return &n }

func TestRatePrice(t *testing.T) {
	flat := domain.Rate{PricingType: domain.PricingFlat, FlatAmountMinor: 7500}
	weight := domain.Rate{PricingType: domain.PricingWeightTiered, WeightTiers: []domain.WeightTier{
		{MaxWeightG: 1000, AmountMinor: 5000}, {MaxWeightG: 5000, AmountMinor: 7500},
	}}
	price := domain.Rate{PricingType: domain.PricingPriceTiered, PriceTiers: []domain.PriceTier{
		{MinSubtotalMinor: 0, AmountMinor: 7500}, {MinSubtotalMinor: 50000, AmountMinor: 0},
	}}
	for _, tc := range []struct {
		name   string
		rate   domain.Rate
		parcel domain.Parcel
		amount int64
		ok     bool
	}{
		{"flat", flat, domain.Parcel{}, 7500, true},
		{"light", weight, domain.Parcel{WeightG: 800, WeightKnown: true}, 5000, true},
		{"tier edge", weight, domain.Parcel{WeightG: 1000, WeightKnown: true}, 5000, true},
		{"heavier", weight, domain.Parcel{WeightG: 1001, WeightKnown: true}, 7500, true},
		{"too heavy", weight, domain.Parcel{WeightG: 5001, WeightKnown: true}, 0, false},
		{"weight unknown", weight, domain.Parcel{WeightG: 10}, 0, false},
		{"small order", price, domain.Parcel{SubtotalMinor: 49999}, 7500, true},
		{"free over 500", price, domain.Parcel{SubtotalMinor: 50000}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			amount, ok := tc.rate.Price(tc.parcel)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.amount, amount)
		})
	}
}

func TestRateValidate(t *testing.T) {
	drivers := []string{"manual"}
	good := domain.Rate{
		Method: "express", DisplayName: "Express", PricingType: domain.PricingWeightTiered, CarrierDriver: "manual",
		WeightTiers:      []domain.WeightTier{{MaxWeightG: 1000, AmountMinor: 9000}},
		EstimatedDaysMin: days(1), EstimatedDaysMax: days(2),
	}
	require.NoError(t, good.Validate(drivers))

	for name, r := range map[string]domain.Rate{
		"method":         {Method: "Express!", DisplayName: "x", PricingType: domain.PricingFlat, CarrierDriver: "manual"},
		"driver":         {Method: "x", DisplayName: "x", PricingType: domain.PricingFlat, CarrierDriver: "dhl"},
		"flat with tier": {Method: "x", DisplayName: "x", PricingType: domain.PricingFlat, CarrierDriver: "manual", PriceTiers: []domain.PriceTier{{}}},
		"no tiers":       {Method: "x", DisplayName: "x", PricingType: domain.PricingWeightTiered, CarrierDriver: "manual"},
		"tiers down": {
			Method: "x", DisplayName: "x", PricingType: domain.PricingWeightTiered, CarrierDriver: "manual",
			WeightTiers: []domain.WeightTier{{MaxWeightG: 500}, {MaxWeightG: 400}},
		},
		"price from 100": {
			Method: "x", DisplayName: "x", PricingType: domain.PricingPriceTiered, CarrierDriver: "manual",
			PriceTiers: []domain.PriceTier{{MinSubtotalMinor: 100}},
		},
		"days": {
			Method: "x", DisplayName: "x", PricingType: domain.PricingFlat, CarrierDriver: "manual",
			EstimatedDaysMin: days(5), EstimatedDaysMax: days(2),
		},
		"type": {Method: "x", DisplayName: "x", PricingType: "zone", CarrierDriver: "manual"},
	} {
		assert.True(t, ycerrors.HasCode(r.Validate(drivers), ycerrors.CodeValidationFailed), name)
	}
}

func TestOptions(t *testing.T) {
	rates := []domain.Rate{
		{ID: "1", Method: "express", DisplayName: "Express", PricingType: domain.PricingFlat, FlatAmountMinor: 15000, Active: true},
		{ID: "2", Method: "standard", DisplayName: "Standard", PricingType: domain.PricingFlat, FlatAmountMinor: 7500, Active: true},
		{ID: "3", Method: "old", DisplayName: "Old", PricingType: domain.PricingFlat, FlatAmountMinor: 100},
		{
			ID: "4", Method: "heavy", DisplayName: "Heavy", PricingType: domain.PricingWeightTiered, Active: true,
			WeightTiers: []domain.WeightTier{{MaxWeightG: 100, AmountMinor: 1}},
		},
	}
	opts := domain.Options(rates, domain.Parcel{WeightG: 500, WeightKnown: true})
	require.Len(t, opts, 2, "inactive and inapplicable rates are left out")
	assert.Equal(t, "standard", opts[0].ID, "cheapest first")
	assert.Equal(t, "2", opts[0].RateID)
	assert.Equal(t, "express", opts[1].ID)
}

func TestShipmentApply(t *testing.T) {
	now := time.Now()
	sh := domain.Shipment{Status: domain.StatusLabelCreated}
	require.True(t, ycerrors.HasCode(sh.Apply(domain.Update{Status: domain.StatusDelivered}, now),
		ycerrors.CodeInvalidStateTransition), "not delivered before it left")
	require.NoError(t, sh.Apply(domain.Update{Status: domain.StatusInTransit, CarrierName: "Courier Guy", CarrierReference: "CG1"}, now))
	require.NoError(t, sh.Apply(domain.Update{Status: domain.StatusInTransit}, now), "in transit repeats")
	assert.Equal(t, "Courier Guy", sh.CarrierName, "kept when not given again")
	require.NoError(t, sh.Apply(domain.Update{Status: domain.StatusException, Message: "nobody home"}, now))
	require.NoError(t, sh.Apply(domain.Update{Status: domain.StatusDelivered}, now))
	assert.False(t, sh.Status.CanMove(domain.StatusInTransit), "delivered is final")
	assert.False(t, domain.StatusInTransit.CanMove(domain.StatusCancelled), "a dispatched parcel is not cancelled")

	assert.Error(t, domain.Update{Status: domain.StatusException}.Validate(), "an exception says what went wrong")
	assert.Error(t, domain.Update{Status: domain.StatusCancelled}.Validate(), "admins do not cancel shipments")
}

func TestTrackingNumber(t *testing.T) {
	n := domain.NewTrackingNumber()
	assert.Len(t, n, 14)
	assert.True(t, domain.ValidTrackingNumber(n))
	assert.NotEqual(t, n, domain.NewTrackingNumber())
	assert.False(t, domain.ValidTrackingNumber("yc1234"), "upper case only")
	assert.False(t, domain.ValidTrackingNumber("YC 1"))
}
