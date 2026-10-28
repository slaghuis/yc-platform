package domain

import (
	"regexp"
	"strings"
	"unicode/utf8"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
)

// BillingAddress is a structured SA billing block for tax invoices
// (issue #29; ADR-015 amendment). Distinct from free-form delivery JSON.
type BillingAddress struct {
	CompanyName     string `json:"company_name,omitempty"`
	VATNumber       string `json:"vat_number,omitempty"`
	StreetAndNumber string `json:"street_and_number"`
	Suburb          string `json:"suburb,omitempty"`
	TownCity        string `json:"town_city"`
	PostalCode      string `json:"postal_code"`
	CountryCode     string `json:"country_code,omitempty"`
}

const (
	maxBillingField = 200
	defaultCountry  = "ZA"
)

var saVATNumber = regexp.MustCompile(`^4\d{9}$`)

// NormaliseBilling trims fields and defaults country_code to ZA.
func NormaliseBilling(b BillingAddress) BillingAddress {
	b.CompanyName = strings.TrimSpace(b.CompanyName)
	b.VATNumber = strings.ToUpper(strings.TrimSpace(b.VATNumber))
	b.StreetAndNumber = strings.TrimSpace(b.StreetAndNumber)
	b.Suburb = strings.TrimSpace(b.Suburb)
	b.TownCity = strings.TrimSpace(b.TownCity)
	b.PostalCode = strings.TrimSpace(b.PostalCode)
	b.CountryCode = strings.ToUpper(strings.TrimSpace(b.CountryCode))
	if b.CountryCode == "" {
		b.CountryCode = defaultCountry
	}
	return b
}

// ValidateBilling checks a billing address when one is supplied.
func ValidateBilling(b BillingAddress) error {
	b = NormaliseBilling(b)
	v := ycerrors.Validation()
	require := func(field, value, msg string) {
		if value == "" {
			v.Field(field, "required", msg)
		}
	}
	require("street_and_number", b.StreetAndNumber, "Street and number is required")
	require("town_city", b.TownCity, "Town or city is required")
	require("postal_code", b.PostalCode, "Postal code is required")
	checkLen := func(field, value string) {
		if utf8.RuneCountInString(value) > maxBillingField {
			v.Field(field, "too_long", "Must be at most 200 characters")
		}
	}
	checkLen("company_name", b.CompanyName)
	checkLen("street_and_number", b.StreetAndNumber)
	checkLen("suburb", b.Suburb)
	checkLen("town_city", b.TownCity)
	checkLen("postal_code", b.PostalCode)
	if b.VATNumber != "" && !saVATNumber.MatchString(b.VATNumber) {
		v.Field("vat_number", "invalid", "VAT number must be 10 digits starting with 4")
	}
	if len(b.CountryCode) != 2 {
		v.Field("country_code", "invalid", "Country code must be ISO 3166-1 alpha-2")
	}
	if err := v.Build(); len(err.Fields) > 0 {
		return err
	}
	return nil
}
