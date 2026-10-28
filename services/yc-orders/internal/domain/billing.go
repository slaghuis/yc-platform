package domain

import (
	"regexp"
	"strings"
	"unicode/utf8"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
)

// BillingAddress is the buyer billing snapshot on an order (issue #29).
type BillingAddress struct {
	CompanyName     string `json:"company_name,omitempty"`
	VATNumber       string `json:"vat_number,omitempty"`
	StreetAndNumber string `json:"street_and_number"`
	Suburb          string `json:"suburb,omitempty"`
	TownCity        string `json:"town_city"`
	PostalCode      string `json:"postal_code"`
	CountryCode     string `json:"country_code,omitempty"`
}

// SellerInvoice is the shop identity snapshotted at order place (issue #29).
type SellerInvoice struct {
	Name           string          `json:"name"`
	ContactEmail   string          `json:"contact_email"`
	VATNumber      string          `json:"vat_number,omitempty"`
	InvoiceAddress *BillingAddress `json:"invoice_address,omitempty"`
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
	if b.StreetAndNumber == "" {
		v.Field("street_and_number", "required", "Street and number is required")
	}
	if b.TownCity == "" {
		v.Field("town_city", "required", "Town or city is required")
	}
	if b.PostalCode == "" {
		v.Field("postal_code", "required", "Postal code is required")
	}
	for field, value := range map[string]string{
		"company_name": b.CompanyName, "street_and_number": b.StreetAndNumber,
		"suburb": b.Suburb, "town_city": b.TownCity, "postal_code": b.PostalCode,
	} {
		if utf8.RuneCountInString(value) > maxBillingField {
			v.Field(field, "too_long", "Must be at most 200 characters")
		}
	}
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
