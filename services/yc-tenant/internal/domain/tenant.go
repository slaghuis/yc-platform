// Package domain holds yc-tenant's types and validation rules (ADR-029).
package domain

import (
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
)

// Status is a tenant's lifecycle state (ADR-029 rule 4).
type Status string

// Lifecycle states: active → suspended → active, and suspended → deleted.
const (
	StatusActive    Status = "active"
	StatusSuspended Status = "suspended"
	StatusDeleted   Status = "deleted"
)

// Valid reports whether s is a lifecycle state.
func (s Status) Valid() bool {
	switch s {
	case StatusActive, StatusSuspended, StatusDeleted:
		return true
	}
	return false
}

// Field limits.
const (
	MaxNameLength   = 200
	MaxReasonLength = 500
	MaxEmailLength  = 254
	MaxHostnameLen  = 253
	MaxHostnames    = 20
	MaxSearchLength = 100
)

// The patterns are those of platform.tenant.provisioned.v1.
var (
	hostnamePattern     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)
	currencyPattern     = regexp.MustCompile(`^[A-Z]{3}$`)
	jurisdictionPattern = regexp.MustCompile(`^[A-Z]{2}$`)
)

// Hostname is one of a tenant's hostnames.
type Hostname struct {
	Name      string
	IsPrimary bool
}

// Tenant is a shop's record.
type Tenant struct {
	TenantID            string
	Name                string
	Status              Status
	StoreCurrency       string
	DefaultJurisdiction string
	IsTaxInclusive      bool
	ContactEmail        string
	// VATNumber and InvoiceAddress are optional seller invoice identity
	// (issue #29; ADR-029 amendment).
	VATNumber      string
	InvoiceAddress *InvoiceAddress
	// Hostnames lists the primary first, then the others by name. A
	// deleted tenant has none: its hostnames are released.
	Hostnames       []Hostname
	CreatedAt       time.Time
	UpdatedAt       time.Time
	SuspendedAt     *time.Time
	SuspendedReason string
	DeletedAt       *time.Time
	DataPurgedAt    *time.Time
}

// PrimaryHostname returns the primary hostname, or "" when there is none.
func (t Tenant) PrimaryHostname() string {
	for _, h := range t.Hostnames {
		if h.IsPrimary {
			return h.Name
		}
	}
	return ""
}

// HostnameNames returns the hostnames with the primary first.
func (t Tenant) HostnameNames() []string {
	return HostnameNames(t.Hostnames)
}

// HostnameNames returns the names of hs in order.
func HostnameNames(hs []Hostname) []string {
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = h.Name
	}
	return out
}

// CreateInput is the body of POST /sysadmin/tenants: the fields of
// platform.tenant.provisioned.v1 plus the contact email.
type CreateInput struct {
	TenantID string
	Name     string
	// Hostnames[0] is the primary.
	Hostnames           []string
	StoreCurrency       string
	DefaultJurisdiction string
	IsTaxInclusive      bool
	ContactEmail        string
	// PrimaryAdminEmail, when set, is invited as the first shop admin.
	PrimaryAdminEmail string
}

// PatchInput is the body of PATCH /sysadmin/tenants/{id}. Nil fields are
// left as they are.
type PatchInput struct {
	Name           *string
	ContactEmail   *string
	VATNumber      *string
	InvoiceAddress *InvoiceAddress
}

// HostnamesInput is the body of PUT /sysadmin/tenants/{id}/hostnames.
type HostnamesInput struct {
	Hostnames []string
	// Primary must be one of Hostnames; empty means Hostnames[0].
	Primary string
}

// ListFilter narrows a listing.
type ListFilter struct {
	// Status, when set, keeps tenants in that state.
	Status Status
	// Search matches the name or a hostname, case-insensitively.
	Search string
}

// ValidateTenantID reports whether id is a valid shop ID (ADR-001). The
// platform pseudo-tenant is not a shop.
func ValidateTenantID(id string) error {
	if id == eventbus.PlatformTenantID {
		return ycerrors.Validation().Field("tenant_id", "reserved", "_platform is reserved").Build()
	}
	if err := eventbus.ValidateTenantID(id); err != nil {
		return ycerrors.Validation().
			Field("tenant_id", "invalid", "Tenant ID must be a lowercase slug of at most 63 characters").Build()
	}
	return nil
}

// NormalizeHostname trims and lowercases a hostname.
func NormalizeHostname(h string) string {
	return strings.ToLower(strings.TrimSpace(h))
}

// ValidHostname reports whether h (already normalised) is a hostname that
// platform.tenant.provisioned.v1 accepts.
func ValidHostname(h string) bool {
	if h == "" || len(h) > MaxHostnameLen || !hostnamePattern.MatchString(h) {
		return false
	}
	for label := range strings.SplitSeq(h, ".") {
		if len(label) > 63 {
			return false
		}
	}
	return true
}

// NormalizeHostnames lowercases hs and checks that each is valid and that
// there are no repeats. field names the input in errors. The result keeps
// the order of hs.
func NormalizeHostnames(field string, hs []string) ([]string, *ycerrors.ValidationBuilder) {
	v := ycerrors.Validation()
	if len(hs) == 0 {
		return nil, v.Field(field, "required", "At least one hostname is required")
	}
	if len(hs) > MaxHostnames {
		return nil, v.Field(field, "too_many", "At most 20 hostnames")
	}
	out := make([]string, 0, len(hs))
	seen := make(map[string]bool, len(hs))
	for _, h := range hs {
		n := NormalizeHostname(h)
		switch {
		case !ValidHostname(n):
			v.Field(field, "invalid", "\""+h+"\" is not a valid hostname")
		case seen[n]:
			v.Field(field, "duplicate", "\""+n+"\" is listed twice")
		default:
			seen[n] = true
			out = append(out, n)
		}
	}
	return out, v
}

// ValidateName trims and checks a display name.
func ValidateName(v *ycerrors.ValidationBuilder, field, name string) string {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		v.Field(field, "required", "Name is required")
	case utf8.RuneCountInString(name) > MaxNameLength:
		v.Field(field, "too_long", "Name is at most 200 characters")
	}
	return name
}

// ValidateEmail trims and checks a bare email address (no display name).
func ValidateEmail(v *ycerrors.ValidationBuilder, field, email string, required bool) string {
	email = strings.TrimSpace(email)
	if email == "" {
		if required {
			v.Field(field, "required", "Email is required")
		}
		return email
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email || len(email) > MaxEmailLength {
		v.Field(field, "invalid", "Email is not valid")
	}
	return email
}

// Validate checks the input and returns it normalised: trimmed names and
// emails, lowercased hostnames.
func (in CreateInput) Validate() (CreateInput, error) {
	hosts, v := NormalizeHostnames("hostnames", in.Hostnames)
	if err := ValidateTenantID(in.TenantID); err != nil {
		if e, ok := ycerrors.As(err); ok {
			for _, f := range e.Fields {
				v.Field(f.Field, f.Code, f.Message)
			}
		}
	}
	out := in
	out.Hostnames = hosts
	out.Name = ValidateName(v, "name", in.Name)
	out.ContactEmail = ValidateEmail(v, "contact_email", in.ContactEmail, true)
	out.PrimaryAdminEmail = ValidateEmail(v, "primary_admin_email", in.PrimaryAdminEmail, false)
	if !currencyPattern.MatchString(in.StoreCurrency) {
		v.Field("store_currency", "invalid", "Store currency is an ISO 4217 code such as ZAR")
	}
	if !jurisdictionPattern.MatchString(in.DefaultJurisdiction) {
		v.Field("default_jurisdiction", "invalid", "Default jurisdiction is an ISO 3166-1 alpha-2 code such as ZA")
	}
	if v.HasErrors() {
		return CreateInput{}, v.Build()
	}
	return out, nil
}

// Validate checks the input and returns it normalised. At least one field
// must be set.
func (in PatchInput) Validate() (PatchInput, error) {
	v := ycerrors.Validation()
	var out PatchInput
	if in.Name == nil && in.ContactEmail == nil && in.VATNumber == nil && in.InvoiceAddress == nil {
		return PatchInput{}, v.Field("body", "required", "Give at least one field to change").Build()
	}
	if in.Name != nil {
		n := ValidateName(v, "name", *in.Name)
		out.Name = &n
	}
	if in.ContactEmail != nil {
		e := ValidateEmail(v, "contact_email", *in.ContactEmail, true)
		out.ContactEmail = &e
	}
	if in.VATNumber != nil {
		vat := NormaliseVAT(*in.VATNumber)
		if err := ValidateVAT(vat); err != nil {
			if e, ok := ycerrors.As(err); ok {
				for _, f := range e.Fields {
					v.Field(f.Field, f.Code, f.Message)
				}
			}
		} else {
			out.VATNumber = &vat
		}
	}
	if in.InvoiceAddress != nil {
		a := NormaliseInvoice(*in.InvoiceAddress)
		if err := ValidateInvoice(a); err != nil {
			if e, ok := ycerrors.As(err); ok {
				for _, f := range e.Fields {
					v.Field(f.Field, f.Code, f.Message)
				}
			}
		} else {
			out.InvoiceAddress = &a
		}
	}
	if v.HasErrors() {
		return PatchInput{}, v.Build()
	}
	return out, nil
}

// Validate checks the input and returns the hostnames with the primary
// first, then the rest in the order given.
func (in HostnamesInput) Validate() ([]Hostname, error) {
	hosts, v := NormalizeHostnames("hostnames", in.Hostnames)
	primary := NormalizeHostname(in.Primary)
	if primary == "" && len(hosts) > 0 {
		primary = hosts[0]
	}
	found := false
	for _, h := range hosts {
		found = found || h == primary
	}
	if !found && !v.HasErrors() {
		v.Field("primary_hostname", "not_listed", "The primary hostname must be one of hostnames")
	}
	if v.HasErrors() {
		return nil, v.Build()
	}
	return OrderHostnames(hosts, primary), nil
}

// OrderHostnames puts primary first and keeps the others in order.
func OrderHostnames(hosts []string, primary string) []Hostname {
	out := make([]Hostname, 0, len(hosts))
	out = append(out, Hostname{Name: primary, IsPrimary: true})
	for _, h := range hosts {
		if h != primary {
			out = append(out, Hostname{Name: h})
		}
	}
	return out
}

// ValidateReason trims and checks a suspension reason.
func ValidateReason(reason string) (string, error) {
	reason = strings.TrimSpace(reason)
	switch {
	case reason == "":
		return "", ycerrors.Validation().Field("reason", "required", "A reason is required").Build()
	case utf8.RuneCountInString(reason) > MaxReasonLength:
		return "", ycerrors.Validation().Field("reason", "too_long", "Reason is at most 500 characters").Build()
	}
	return reason, nil
}

// Validate checks a listing filter and returns it normalised.
func (f ListFilter) Validate() (ListFilter, error) {
	f.Search = strings.TrimSpace(f.Search)
	v := ycerrors.Validation()
	if f.Status != "" && !f.Status.Valid() {
		v.Field("status", "invalid", "Status is active, suspended or deleted")
	}
	if utf8.RuneCountInString(f.Search) > MaxSearchLength {
		v.Field("q", "too_long", "Search is at most 100 characters")
	}
	if v.HasErrors() {
		return ListFilter{}, v.Build()
	}
	return f, nil
}
