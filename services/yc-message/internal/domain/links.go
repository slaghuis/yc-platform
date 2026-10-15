package domain

import (
	"net/url"
	"strings"
)

// ShopLinks builds links to storefront pages. The paths are a contract with
// the storefront (ADR-027 rule 12): changing one breaks links in emails
// already sent.
type ShopLinks struct {
	Base string
}

// NewShopLinks returns the links for a shop whose storefront is at base.
func NewShopLinks(base string) ShopLinks {
	return ShopLinks{Base: strings.TrimSuffix(base, "/")}
}

// Home is the storefront's home page.
func (l ShopLinks) Home() string { return l.Base }

// Order is an order's page.
func (l ShopLinks) Order(orderID string) string {
	return l.Base + "/orders/" + url.PathEscape(orderID)
}

// Cart is the shopping cart.
func (l ShopLinks) Cart() string { return l.Base + "/cart" }

// Review is a product's page, at its review form.
func (l ShopLinks) Review(productID string) string {
	return l.Base + "/products/" + url.PathEscape(productID) + "#review"
}

// Tracking is a shipment's tracking page.
func (l ShopLinks) Tracking(trackingNumber string) string {
	return l.Base + "/shipping/tracking/" + url.PathEscape(trackingNumber)
}
