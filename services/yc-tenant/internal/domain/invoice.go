package domain

import (
	"regexp"
	"strings"
	"unicode/utf8"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
)

// InvoiceAddress is the seller postal block on a tax invoice (issue #29).
// Same postal fields as customer billing, without company/VAT.
type InvoiceAddress struct {
	StreetAndNumber string `json:"street_and_number"`
	Suburb          string `json:"suburb,omitempty"`
	TownCity        string `json:"town_city"`
	PostalCode      string `json:"postal_code"`
	CountryCode     string `json:"country_code,omitempty"`
}

const (
	maxInvoiceField = 200
	defaultCountry  = "ZA"
)

var saVATNumber = regexp.MustCompile(`^4\d{9}$`)

// ShopPatch is PATCH /admin/shop (and the invoice fields on sysadmin PATCH).
// Nil fields are left unchanged. An empty vat_number clears VAT. An empty
// invoice_address (all postal fields blank) clears the stored address.
type ShopPatch struct {
	VATNumber      *string
	InvoiceAddress *InvoiceAddress
}

// NormaliseInvoice trims fields and defaults country_code to ZA when any
// postal field is set.
func NormaliseInvoice(a InvoiceAddress) InvoiceAddress {
	a.StreetAndNumber = strings.TrimSpace(a.StreetAndNumber)
	a.Suburb = strings.TrimSpace(a.Suburb)
	a.TownCity = strings.TrimSpace(a.TownCity)
	a.PostalCode = strings.TrimSpace(a.PostalCode)
	a.CountryCode = strings.ToUpper(strings.TrimSpace(a.CountryCode))
	if !a.Empty() && a.CountryCode == "" {
		a.CountryCode = defaultCountry
	}
	return a
}

// Empty reports whether a has no postal content (used to clear on PATCH).
func (a InvoiceAddress) Empty() bool {
	return a.StreetAndNumber == "" && a.Suburb == "" && a.TownCity == "" &&
		a.PostalCode == "" && a.CountryCode == ""
}

// ValidateInvoice checks a seller invoice address when one is supplied
// (non-empty). Empty addresses are valid (clear).
func ValidateInvoice(a InvoiceAddress) error {
	a = NormaliseInvoice(a)
	if a.Empty() {
		return nil
	}
	v := ycerrors.Validation()
	if a.StreetAndNumber == "" {
		v.Field("invoice_address.street_and_number", "required", "Street and number is required")
	}
	if a.TownCity == "" {
		v.Field("invoice_address.town_city", "required", "Town or city is required")
	}
	if a.PostalCode == "" {
		v.Field("invoice_address.postal_code", "required", "Postal code is required")
	}
	for field, value := range map[string]string{
		"invoice_address.street_and_number": a.StreetAndNumber,
		"invoice_address.suburb":            a.Suburb,
		"invoice_address.town_city":         a.TownCity,
		"invoice_address.postal_code":       a.PostalCode,
	} {
		if utf8.RuneCountInString(value) > maxInvoiceField {
			v.Field(field, "too_long", "Must be at most 200 characters")
		}
	}
	if len(a.CountryCode) != 2 {
		v.Field("invoice_address.country_code", "invalid", "Country code must be ISO 3166-1 alpha-2")
	}
	if err := v.Build(); len(err.Fields) > 0 {
		return err
	}
	return nil
}

// NormaliseVAT trims and uppercases a store VAT number. Empty is allowed.
func NormaliseVAT(vat string) string {
	return strings.ToUpper(strings.TrimSpace(vat))
}

// ValidateVAT soft-checks an SA VAT number when set.
func ValidateVAT(vat string) error {
	vat = NormaliseVAT(vat)
	if vat == "" {
		return nil
	}
	if !saVATNumber.MatchString(vat) {
		return ycerrors.Validation().
			Field("vat_number", "invalid", "VAT number must be 10 digits starting with 4").Build()
	}
	return nil
}

// Validate normalises and checks a shop invoice PATCH.
func (in ShopPatch) Validate() (ShopPatch, error) {
	if in.VATNumber == nil && in.InvoiceAddress == nil {
		return ShopPatch{}, ycerrors.Validation().
			Field("body", "required", "Give vat_number or invoice_address").Build()
	}
	out := ShopPatch{}
	if in.VATNumber != nil {
		v := NormaliseVAT(*in.VATNumber)
		if err := ValidateVAT(v); err != nil {
			return ShopPatch{}, err
		}
		out.VATNumber = &v
	}
	if in.InvoiceAddress != nil {
		a := NormaliseInvoice(*in.InvoiceAddress)
		if err := ValidateInvoice(a); err != nil {
			return ShopPatch{}, err
		}
		out.InvoiceAddress = &a
	}
	return out, nil
}
