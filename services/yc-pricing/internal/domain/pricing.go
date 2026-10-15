// Package domain holds yc-pricing's model and its evaluation pipeline
// (docs/services/yc-pricing.md, ADR-014, ADR-016). It does no I/O.
package domain

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	money "github.com/slaghuis/yc-platform/libs/yc-money"
)

// Limits on tax class definitions.
const (
	MaxDisplayName = 100
	MaxRules       = 50
	MaxRuleName    = 50
)

// BaseLineTotal taxes the line's pre-tax total. A rule with base
// BaseIncludingPrefix+"<rule>" also taxes the named rule's amount
// (compound tax, ADR-016).
const (
	BaseLineTotal       = "line_total"
	BaseIncludingPrefix = "line_total_including_"
)

var (
	classIDPattern      = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	jurisdictionPattern = regexp.MustCompile(`^[A-Z]{2}(-[A-Z0-9]{1,3})?$`)
	ratePattern         = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
	maxRate             = money.MustRate("10")
)

// Config is a shop's pricing configuration.
type Config struct {
	TenantID            string
	StoreCurrency       string
	TaxInclusive        bool
	DefaultJurisdiction string
	UpdatedAt           time.Time
}

// ConfigInput is a shop admin's change to the configuration.
type ConfigInput struct {
	StoreCurrency       string
	TaxInclusive        bool
	DefaultJurisdiction string
}

// Validate checks the currency and jurisdiction.
func (in ConfigInput) Validate() error {
	v := ycerrors.Validation()
	if !money.ValidCurrency(in.StoreCurrency) {
		v.Field("store_currency", "invalid", "Must be an ISO 4217 code")
	}
	if !jurisdictionPattern.MatchString(in.DefaultJurisdiction) {
		v.Field("default_jurisdiction", "invalid", "Must be an ISO 3166 country or subdivision code, e.g. ZA")
	}
	if v.HasErrors() {
		return v.Build()
	}
	return nil
}

// BasePrice is a SKU's current price in the store currency. TaxInclusive
// records how the price was entered; evaluation honours it even if the
// shop's setting has changed since.
type BasePrice struct {
	SKUID        string
	Price        money.Money
	TaxInclusive bool
	ValidFrom    time.Time
}

// PricePeriod is one entry of a SKU's price history. ValidTo is nil for
// the current price.
type PricePeriod struct {
	Price        money.Money
	TaxInclusive bool
	ValidFrom    time.Time
	ValidTo      *time.Time
	SetBy        string
}

// ValidatePrice checks a base price against the store currency.
func ValidatePrice(p money.Money, storeCurrency string) error {
	v := ycerrors.Validation()
	if p.Currency != storeCurrency {
		v.Field("price.currency", "invalid", "Must be the store currency "+storeCurrency)
	}
	if p.AmountMinor < 0 {
		v.Field("price.amount_minor", "invalid", "Must not be negative")
	}
	if v.HasErrors() {
		return v.Build()
	}
	return nil
}

// TaxRule is one tax a class levies in a jurisdiction.
type TaxRule struct {
	Jurisdiction string
	Name         string
	Rate         money.Rate
	Base         string
	SortOrder    int
}

// TaxClass groups the rules applied to SKUs of that class.
type TaxClass struct {
	ID          string
	DisplayName string
	Rules       []TaxRule
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// RulesFor returns the rules for jurisdiction, falling back to its country
// for a subdivision (ZA-WC → ZA), in evaluation order.
func (c TaxClass) RulesFor(jurisdiction string) []TaxRule {
	match := func(j string) []TaxRule {
		var out []TaxRule
		for _, r := range c.Rules {
			if r.Jurisdiction == j {
				out = append(out, r)
			}
		}
		return out
	}
	if rules := match(jurisdiction); len(rules) > 0 {
		return rules
	}
	if country, _, ok := strings.Cut(jurisdiction, "-"); ok {
		return match(country)
	}
	return nil
}

// TaxRuleInput is a rule as submitted, with the rate as a decimal string.
type TaxRuleInput struct {
	Jurisdiction string
	Name         string
	Rate         string
	Base         string
	SortOrder    int
}

// TaxClassInput defines (or redefines) a tax class.
type TaxClassInput struct {
	DisplayName string
	Rules       []TaxRuleInput
}

// ValidateClassID checks a tax class ID.
func ValidateClassID(id string) error {
	if !classIDPattern.MatchString(id) {
		return ycerrors.Validation().Field("class_id", "invalid", "Must be lower-case words joined by hyphens, e.g. za-standard").Build()
	}
	return nil
}

// Validate checks the class and returns its rules in evaluation order:
// by jurisdiction, then sort order. A compound rule may only include a
// rule of the same jurisdiction that is evaluated before it.
func (in TaxClassInput) Validate() ([]TaxRule, error) {
	v := ycerrors.Validation()
	if n := len(in.DisplayName); n == 0 || n > MaxDisplayName {
		v.Field("display_name", "invalid", fmt.Sprintf("Must be 1-%d characters", MaxDisplayName))
	}
	if len(in.Rules) > MaxRules {
		v.Field("rules", "too_many", fmt.Sprintf("At most %d rules", MaxRules))
	}
	rules := make([]TaxRule, 0, len(in.Rules))
	for i, r := range in.Rules {
		f := fmt.Sprintf("rules[%d].", i)
		if !jurisdictionPattern.MatchString(r.Jurisdiction) {
			v.Field(f+"jurisdiction", "invalid", "Must be an ISO 3166 country or subdivision code, e.g. ZA")
		}
		if n := len(r.Name); n == 0 || n > MaxRuleName || strings.TrimSpace(r.Name) != r.Name {
			v.Field(f+"rule_name", "invalid", fmt.Sprintf("Must be 1-%d characters without surrounding spaces", MaxRuleName))
		}
		var rate money.Rate
		if ratePattern.MatchString(r.Rate) {
			rate = money.MustRate(r.Rate)
		}
		if !ratePattern.MatchString(r.Rate) || rate.Cmp(maxRate) > 0 {
			v.Field(f+"rate", "invalid", "Must be a decimal between 0 and 10, e.g. \"0.15\"")
		}
		rules = append(rules, TaxRule{Jurisdiction: r.Jurisdiction, Name: r.Name, Rate: rate, Base: r.Base, SortOrder: r.SortOrder})
	}
	if v.HasErrors() {
		return nil, v.Build()
	}

	slices.SortStableFunc(rules, func(a, b TaxRule) int {
		return cmp.Or(strings.Compare(a.Jurisdiction, b.Jurisdiction), cmp.Compare(a.SortOrder, b.SortOrder))
	})
	seen := map[string]bool{}
	for i, r := range rules {
		key := r.Jurisdiction + "/" + r.Name
		if seen[key] {
			v.Field("rules", "duplicate", fmt.Sprintf("Rule %q appears twice for %s", r.Name, r.Jurisdiction))
		}
		if i > 0 && rules[i-1].Jurisdiction == r.Jurisdiction && rules[i-1].SortOrder == r.SortOrder {
			v.Field("rules", "ambiguous_order", fmt.Sprintf("Rules for %s need distinct sort_order values", r.Jurisdiction))
		}
		switch ref, compound := strings.CutPrefix(r.Base, BaseIncludingPrefix); {
		case r.Base == BaseLineTotal:
		case compound && seen[r.Jurisdiction+"/"+ref]:
		default:
			v.Field("rules", "invalid_base", fmt.Sprintf(
				"Rule %q: base must be %s or %s<rule> naming an earlier rule for %s", r.Name, BaseLineTotal, BaseIncludingPrefix, r.Jurisdiction))
		}
		seen[key] = true
	}
	if v.HasErrors() {
		return nil, v.Build()
	}
	return rules, nil
}

// ExchangeRate is the latest rate from Base to Quote: one unit of Base buys
// Rate units of Quote.
type ExchangeRate struct {
	Base      string
	Quote     string
	Rate      money.Rate
	Provider  string
	FetchedAt time.Time
}
