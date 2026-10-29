package domain

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	money "github.com/slaghuis/yc-platform/libs/yc-money"
)

// Discount types applied by promotion evaluators.
const (
	DiscountTypeSalePrice = "sale_price"
	DiscountTypeBXGY      = "bxgy"
	DiscountKindPercent   = "percent"
	DiscountKindFixed     = "fixed"
)

// LineDiscount is one reduction attributed on a priced line (before tax).
type LineDiscount struct {
	Type   string // sale_price | bxgy
	Name   string
	Amount money.Money // negative of the reduction (stored as positive reduction in AmountMinor via Reduction)
}

// SalePrice is a time-bounded promotional unit price for a SKU.
type SalePrice struct {
	ID           string
	SKUID        string
	Price        money.Money
	TaxInclusive bool
	Name         string
	StartsAt     time.Time
	EndsAt       *time.Time
	UpdatedAt    time.Time
}

// SalePriceInput is the admin body for PUT sale price.
type SalePriceInput struct {
	Price    money.Money
	Name     string
	StartsAt time.Time
	EndsAt   *time.Time
}

// Validate checks the sale overlay against the store currency.
func (in SalePriceInput) Validate(storeCurrency string) error {
	v := ycerrors.Validation()
	if err := ValidatePrice(in.Price, storeCurrency); err != nil {
		return err
	}
	if in.StartsAt.IsZero() {
		v.Field("starts_at", "required", "Required")
	}
	if in.EndsAt != nil && !in.EndsAt.After(in.StartsAt) {
		v.Field("ends_at", "invalid", "Must be after starts_at")
	}
	if len(in.Name) > 100 {
		v.Field("name", "too_long", "At most 100 characters")
	}
	if v.HasErrors() {
		return v.Build()
	}
	return nil
}

// ActiveAt reports whether the sale applies at t.
func (s SalePrice) ActiveAt(t time.Time) bool {
	if t.Before(s.StartsAt) {
		return false
	}
	return s.EndsAt == nil || t.Before(*s.EndsAt)
}

// BXGYCampaign is buy-X-get-Y: qualify on A, discount target B.
type BXGYCampaign struct {
	ID                  string
	Name                string
	QualifyingProductID string
	QualifyingSKUID     string
	TargetProductID     string
	TargetSKUID         string
	DiscountType        string // percent | fixed
	DiscountValue       int64  // percent in basis points (10000 = 100%) or fixed minor units
	StartsAt            time.Time
	EndsAt              *time.Time
	Enabled             bool
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// BXGYInput is create/update body.
type BXGYInput struct {
	Name                string
	QualifyingProductID string
	QualifyingSKUID     string
	TargetProductID     string
	TargetSKUID         string
	DiscountType        string
	DiscountValue       int64
	StartsAt            time.Time
	EndsAt              *time.Time
	Enabled             *bool
}

// Validate checks XOR selectors and discount bounds.
func (in BXGYInput) Validate() error {
	v := ycerrors.Validation()
	if in.Name == "" || len(in.Name) > 100 {
		v.Field("name", "invalid", "Required, at most 100 characters")
	}
	qProd, qSKU := in.QualifyingProductID != "", in.QualifyingSKUID != ""
	if qProd == qSKU {
		v.Field("qualifying", "invalid", "Give exactly one of qualifying_product_id or qualifying_sku_id")
	}
	tProd, tSKU := in.TargetProductID != "", in.TargetSKUID != ""
	if tProd == tSKU {
		v.Field("target", "invalid", "Give exactly one of target_product_id or target_sku_id")
	}
	for field, id := range map[string]string{
		"qualifying_product_id": in.QualifyingProductID, "qualifying_sku_id": in.QualifyingSKUID,
		"target_product_id": in.TargetProductID, "target_sku_id": in.TargetSKUID,
	} {
		if id == "" {
			continue
		}
		if _, err := uuid.Parse(id); err != nil {
			v.Field(field, "invalid", "Must be a UUID")
		}
	}
	switch in.DiscountType {
	case DiscountKindPercent:
		if in.DiscountValue < 1 || in.DiscountValue > 10000 {
			v.Field("discount_value", "invalid", "Percent must be 1-10000 basis points (10000 = 100%)")
		}
	case DiscountKindFixed:
		if in.DiscountValue < 1 {
			v.Field("discount_value", "invalid", "Fixed discount must be at least 1 minor unit")
		}
	default:
		v.Field("discount_type", "invalid", "Must be percent or fixed")
	}
	if in.StartsAt.IsZero() {
		v.Field("starts_at", "required", "Required")
	}
	if in.EndsAt != nil && !in.EndsAt.After(in.StartsAt) {
		v.Field("ends_at", "invalid", "Must be after starts_at")
	}
	if v.HasErrors() {
		return v.Build()
	}
	return nil
}

// ActiveAt reports whether the campaign applies at t.
func (c BXGYCampaign) ActiveAt(t time.Time) bool {
	if !c.Enabled || t.Before(c.StartsAt) {
		return false
	}
	return c.EndsAt == nil || t.Before(*c.EndsAt)
}

// MatchesQualifying reports whether sku (of product) is the campaign qualifier.
func (c BXGYCampaign) MatchesQualifying(skuID, productID string) bool {
	if c.QualifyingSKUID != "" {
		return c.QualifyingSKUID == skuID
	}
	return c.QualifyingProductID != "" && c.QualifyingProductID == productID
}

// MatchesTarget reports whether sku (of product) is the campaign target.
func (c BXGYCampaign) MatchesTarget(skuID, productID string) bool {
	if c.TargetSKUID != "" {
		return c.TargetSKUID == skuID
	}
	return c.TargetProductID != "" && c.TargetProductID == productID
}

// DiscountAmount returns how much to take off lineGross (pre-tax line amount).
func (c BXGYCampaign) DiscountAmount(lineGross money.Money) (money.Money, error) {
	switch c.DiscountType {
	case DiscountKindPercent:
		// discount_value is basis points; 10000 = 100%.
		parts := lineGross.AmountMinor * c.DiscountValue
		off := parts / 10000
		if parts%10000 >= 5000 {
			off++
		}
		if off > lineGross.AmountMinor {
			off = lineGross.AmountMinor
		}
		return money.New(off, lineGross.Currency), nil
	case DiscountKindFixed:
		off := c.DiscountValue
		if off > lineGross.AmountMinor {
			off = lineGross.AmountMinor
		}
		return money.New(off, lineGross.Currency), nil
	default:
		return money.Money{}, fmt.Errorf("unknown discount type %q", c.DiscountType)
	}
}
