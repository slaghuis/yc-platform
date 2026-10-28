package integration

import (
	"context"
	"net/http"
	"net/url"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
)

// BillingAddress is structured SA billing (issue #29).
type BillingAddress struct {
	CompanyName     string `json:"company_name,omitempty"`
	VATNumber       string `json:"vat_number,omitempty"`
	StreetAndNumber string `json:"street_and_number"`
	Suburb          string `json:"suburb,omitempty"`
	TownCity        string `json:"town_city"`
	PostalCode      string `json:"postal_code"`
	CountryCode     string `json:"country_code,omitempty"`
}

// SellerInvoice is the shop identity for an invoice snapshot.
type SellerInvoice struct {
	Name           string          `json:"name"`
	ContactEmail   string          `json:"contact_email"`
	VATNumber      string          `json:"vat_number,omitempty"`
	InvoiceAddress *BillingAddress `json:"invoice_address,omitempty"`
}

// Identity loads customer billing from yc-identity.
type Identity struct {
	client Caller
}

// NewIdentity returns an Identity client.
func NewIdentity(client Caller) *Identity {
	return &Identity{client: client}
}

// UserBilling returns a customer's saved billing, or ok=false when none.
func (i *Identity) UserBilling(ctx context.Context, userID string) (BillingAddress, bool, error) {
	var out BillingAddress
	err := i.client.Call(ctx, http.MethodGet, "/internal/users/"+url.PathEscape(userID)+"/billing",
		httpkit.CallOptions{Attempts: 2}, nil, &out)
	if err != nil {
		if e, ok := ycerrors.As(err); ok && e.Code == ycerrors.CodeNotFound {
			return BillingAddress{}, false, nil
		}
		return BillingAddress{}, false, err
	}
	return out, true, nil
}

// Tenant loads shop invoice identity from yc-tenant.
type Tenant struct {
	client Caller
}

// NewTenant returns a Tenant client.
func NewTenant(client Caller) *Tenant {
	return &Tenant{client: client}
}

// SellerInvoice returns the shop's invoice identity for tenantID.
func (t *Tenant) SellerInvoice(ctx context.Context, tenantID string) (SellerInvoice, error) {
	var out SellerInvoice
	err := t.client.Call(ctx, http.MethodGet, "/internal/tenants/"+url.PathEscape(tenantID),
		httpkit.CallOptions{Attempts: 2}, nil, &out)
	return out, err
}
