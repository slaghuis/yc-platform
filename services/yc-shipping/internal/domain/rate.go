package domain

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
)

// PricingType is how a rate's amount is worked out.
type PricingType string

// Pricing types.
const (
	PricingFlat         PricingType = "flat"
	PricingWeightTiered PricingType = "weight_tiered"
	PricingPriceTiered  PricingType = "price_tiered"
)

// MaxTiers bounds a rate's tiers.
const MaxTiers = 20

// WeightTier prices parcels up to MaxWeightG grams.
type WeightTier struct {
	MaxWeightG  int64 `json:"max_weight_g"`
	AmountMinor int64 `json:"amount_minor"`
}

// PriceTier prices orders whose items total is at least MinSubtotalMinor.
type PriceTier struct {
	MinSubtotalMinor int64 `json:"min_subtotal_minor"`
	AmountMinor      int64 `json:"amount_minor"`
}

// Rate is one shipping method in a zone. Amounts are in the store
// currency (ADR-014).
type Rate struct {
	ID                   string
	ZoneID               string
	Method               string
	DisplayName          string
	PricingType          PricingType
	FlatAmountMinor      int64
	WeightTiers          []WeightTier
	PriceTiers           []PriceTier
	EstimatedDaysMin     *int
	EstimatedDaysMax     *int
	CarrierDriver        string
	ScheduleID           string // empty when no delivery schedule
	RequiresDeliverySlot bool
	Active               bool
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

var methodRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// Validate checks a rate from a shop admin. drivers are the carrier
// drivers yc-shipping has.
func (r Rate) Validate(drivers []string) error {
	v := ycerrors.Validation()
	if !methodRE.MatchString(r.Method) {
		v.Field("method", "invalid", "Method is 1 to 32 lower-case letters, digits, - or _")
	}
	if name := strings.TrimSpace(r.DisplayName); name == "" || len(name) > 100 {
		v.Field("display_name", "invalid", "Display name is 1 to 100 characters")
	}
	if !slices.Contains(drivers, r.CarrierDriver) {
		v.Field("carrier_driver", "invalid", "Carrier driver must be one of "+strings.Join(drivers, ", "))
	}
	switch r.PricingType {
	case PricingFlat:
		if r.FlatAmountMinor < 0 {
			v.Field("flat_amount_minor", "invalid", "Amount must not be negative")
		}
		if len(r.WeightTiers) > 0 || len(r.PriceTiers) > 0 {
			v.Field("pricing_type", "invalid", "A flat rate has no tiers")
		}
	case PricingWeightTiered:
		validateWeightTiers(v, r.WeightTiers)
		if len(r.PriceTiers) > 0 {
			v.Field("price_tiers", "invalid", "A weight-tiered rate has no price tiers")
		}
	case PricingPriceTiered:
		validatePriceTiers(v, r.PriceTiers)
		if len(r.WeightTiers) > 0 {
			v.Field("weight_tiers", "invalid", "A price-tiered rate has no weight tiers")
		}
	default:
		v.Field("pricing_type", "invalid", "Pricing type is flat, weight_tiered or price_tiered")
	}
	lo, hi := r.EstimatedDaysMin, r.EstimatedDaysMax
	if (lo != nil && (*lo < 0 || *lo > 365)) || (hi != nil && (*hi < 0 || *hi > 365)) {
		v.Field("estimated_days_min", "invalid", "Estimated days are between 0 and 365")
	} else if lo != nil && hi != nil && *lo > *hi {
		v.Field("estimated_days_max", "invalid", "The most days must not be fewer than the least")
	}
	if r.RequiresDeliverySlot && r.ScheduleID == "" {
		v.Field("schedule_id", "required", "A rate that requires a delivery slot needs a schedule")
	}
	if r.ScheduleID != "" {
		if _, err := uuid.Parse(r.ScheduleID); err != nil {
			v.Field("schedule_id", "invalid", "Schedule ID must be a UUID")
		}
	}
	if v.HasErrors() {
		return v.Build()
	}
	return nil
}

func validateWeightTiers(v *ycerrors.ValidationBuilder, tiers []WeightTier) {
	if len(tiers) == 0 || len(tiers) > MaxTiers {
		v.Field("weight_tiers", "invalid", fmt.Sprintf("A weight-tiered rate has 1 to %d tiers", MaxTiers))
		return
	}
	var prev int64
	for i, t := range tiers {
		if t.MaxWeightG <= prev {
			v.Field(fmt.Sprintf("weight_tiers[%d].max_weight_g", i), "invalid", "Tiers go up in weight, from more than 0 g")
		}
		if t.AmountMinor < 0 {
			v.Field(fmt.Sprintf("weight_tiers[%d].amount_minor", i), "invalid", "Amount must not be negative")
		}
		prev = t.MaxWeightG
	}
}

func validatePriceTiers(v *ycerrors.ValidationBuilder, tiers []PriceTier) {
	if len(tiers) == 0 || len(tiers) > MaxTiers {
		v.Field("price_tiers", "invalid", fmt.Sprintf("A price-tiered rate has 1 to %d tiers", MaxTiers))
		return
	}
	for i, t := range tiers {
		switch {
		case i == 0 && t.MinSubtotalMinor != 0:
			v.Field("price_tiers[0].min_subtotal_minor", "invalid", "The first tier starts at 0")
		case i > 0 && t.MinSubtotalMinor <= tiers[i-1].MinSubtotalMinor:
			v.Field(fmt.Sprintf("price_tiers[%d].min_subtotal_minor", i), "invalid", "Tiers go up in subtotal")
		}
		if t.AmountMinor < 0 {
			v.Field(fmt.Sprintf("price_tiers[%d].amount_minor", i), "invalid", "Amount must not be negative")
		}
	}
}

// Parcel is what a quote prices: the physical items' weight and the
// order's items total.
type Parcel struct {
	WeightG int64
	// WeightKnown is false when a physical item has no weight.
	WeightKnown   bool
	SubtotalMinor int64
}

// Price returns the rate's amount for p, or false when the rate does not
// apply (no weight to price by, or heavier than its last tier).
func (r Rate) Price(p Parcel) (int64, bool) {
	switch r.PricingType {
	case PricingFlat:
		return r.FlatAmountMinor, true
	case PricingWeightTiered:
		if !p.WeightKnown {
			return 0, false
		}
		for _, t := range r.WeightTiers {
			if p.WeightG <= t.MaxWeightG {
				return t.AmountMinor, true
			}
		}
	case PricingPriceTiered:
		amount, ok := int64(0), false
		for _, t := range r.PriceTiers {
			if p.SubtotalMinor >= t.MinSubtotalMinor {
				amount, ok = t.AmountMinor, true
			}
		}
		return amount, ok
	}
	return 0, false
}

// Option is a priced rate offered to checkout.
type Option struct {
	ID                   string
	RateID               string
	Name                 string
	AmountMinor          int64
	EstimatedDaysMin     *int
	EstimatedDaysMax     *int
	CarrierDriver        string
	ScheduleID           string
	RequiresDeliverySlot bool
}

// Options prices the zone's active rates for p, cheapest first. Rates that
// do not apply are left out.
func Options(rates []Rate, p Parcel) []Option {
	out := make([]Option, 0, len(rates))
	for _, r := range rates {
		if !r.Active {
			continue
		}
		amount, ok := r.Price(p)
		if !ok {
			continue
		}
		out = append(out, Option{
			ID: r.Method, RateID: r.ID, Name: r.DisplayName, AmountMinor: amount,
			EstimatedDaysMin: r.EstimatedDaysMin, EstimatedDaysMax: r.EstimatedDaysMax, CarrierDriver: r.CarrierDriver,
			ScheduleID: r.ScheduleID, RequiresDeliverySlot: r.RequiresDeliverySlot,
		})
	}
	slices.SortStableFunc(out, func(a, b Option) int {
		if a.AmountMinor != b.AmountMinor {
			if a.AmountMinor < b.AmountMinor {
				return -1
			}
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}
