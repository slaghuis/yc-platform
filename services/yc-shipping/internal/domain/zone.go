// Package domain holds yc-shipping's zones, rates and shipments and their
// rules.
package domain

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
)

// Zone priorities: zones are matched lowest first.
const (
	DefaultPriority = 100
	MaxPriority     = 1000
)

// Bounds on a zone's rules.
const (
	MaxRules      = 50
	MaxRuleFields = 10
)

// Rule matches addresses in one country, optionally narrowed by exact
// values of the address's fields (ADR-015), e.g. province Gauteng.
type Rule struct {
	CountryCode string            `json:"country_code"`
	Fields      map[string]string `json:"fields,omitempty"`
}

// Zone is a shop-defined group of regions.
type Zone struct {
	ID        string
	Name      string
	Rules     []Rule
	Priority  int
	CreatedAt time.Time
	UpdatedAt time.Time
}

var (
	countryRE  = regexp.MustCompile(`^[A-Z]{2}$`)
	fieldKeyRE = regexp.MustCompile(`^[A-Za-z0-9_]{1,50}$`)
)

// Normalise upper-cases country codes and trims names and values.
func (z *Zone) Normalise() {
	z.Name = strings.TrimSpace(z.Name)
	for i := range z.Rules {
		z.Rules[i].CountryCode = strings.ToUpper(strings.TrimSpace(z.Rules[i].CountryCode))
		for k, v := range z.Rules[i].Fields {
			z.Rules[i].Fields[k] = strings.TrimSpace(v)
		}
	}
}

// Validate checks a zone from a shop admin.
func (z Zone) Validate() error {
	v := ycerrors.Validation()
	if z.Name == "" || len(z.Name) > 100 {
		v.Field("name", "invalid", "Name is 1 to 100 characters")
	}
	if z.Priority < 0 || z.Priority > MaxPriority {
		v.Field("priority", "invalid", fmt.Sprintf("Priority is between 0 and %d", MaxPriority))
	}
	if len(z.Rules) == 0 || len(z.Rules) > MaxRules {
		v.Field("match_rules", "invalid", fmt.Sprintf("A zone has 1 to %d rules", MaxRules))
	}
	for i, r := range z.Rules {
		f := fmt.Sprintf("match_rules[%d]", i)
		if !countryRE.MatchString(r.CountryCode) {
			v.Field(f+".country_code", "invalid", "Country code is ISO 3166-1 alpha-2")
		}
		if len(r.Fields) > MaxRuleFields {
			v.Field(f+".fields", "invalid", fmt.Sprintf("A rule matches at most %d fields", MaxRuleFields))
		}
		for k, val := range r.Fields {
			if !fieldKeyRE.MatchString(k) {
				v.Field(f+".fields."+k, "invalid", "Field names are letters, digits and underscores")
			}
			if val == "" || len(val) > 100 {
				v.Field(f+".fields."+k, "invalid", "Field values are 1 to 100 characters")
			}
		}
	}
	if v.HasErrors() {
		return v.Build()
	}
	return nil
}

// Matches reports whether the zone covers addr: any of its rules does.
func (z Zone) Matches(addr map[string]any) bool {
	for _, r := range z.Rules {
		if r.Matches(addr) {
			return true
		}
	}
	return false
}

// Matches reports whether addr is in the rule's country and has each of
// its field values (case-insensitive). The country is the address's
// country_code; the fields are under its fields object (ADR-015).
func (r Rule) Matches(addr map[string]any) bool {
	country, _ := addr["country_code"].(string)
	if !strings.EqualFold(strings.TrimSpace(country), r.CountryCode) {
		return false
	}
	if len(r.Fields) == 0 {
		return true
	}
	fields, _ := addr["fields"].(map[string]any)
	for k, want := range r.Fields {
		got, _ := fields[k].(string)
		if !strings.EqualFold(strings.TrimSpace(got), want) {
			return false
		}
	}
	return true
}

// ResolveZone returns the first zone, in priority order, that covers addr.
// zones must be sorted by priority.
func ResolveZone(zones []Zone, addr map[string]any) (Zone, bool) {
	for _, z := range zones {
		if z.Matches(addr) {
			return z, true
		}
	}
	return Zone{}, false
}
