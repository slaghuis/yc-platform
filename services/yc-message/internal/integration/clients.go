// Package integration calls other services' internal APIs through
// yc-gateway (ADR-021): yc-preference for how to reach a customer and
// yc-orders for order details.
package integration

import (
	"context"
	"net/http"
	"net/url"
	"time"

	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
)

// Caller makes internal calls.
type Caller interface {
	Call(ctx context.Context, method, path string, opts httpkit.CallOptions, in, out any) error
}

// Clients are yc-message's upstreams.
type Clients struct {
	client Caller
}

// New returns the clients.
func New(client Caller) *Clients {
	return &Clients{client: client}
}

// ChannelPreference is one of a user's messaging channels.
type ChannelPreference struct {
	Channel  string `json:"channel"`
	Address  string `json:"address"`
	Verified bool   `json:"verified"`
}

// Preferences is how a customer wants to be reached.
type Preferences struct {
	UserID         string              `json:"user_id"`
	Locale         string              `json:"locale"`
	PrimaryChannel string              `json:"primary_channel"`
	Newsletter     bool                `json:"newsletter"`
	Marketing      bool                `json:"marketing"`
	NewProducts    bool                `json:"new_products"`
	Channels       []ChannelPreference `json:"channels"`
}

// Preferences returns a user's messaging preferences.
func (c *Clients) Preferences(ctx context.Context, userID string) (Preferences, error) {
	var out Preferences
	err := c.client.Call(ctx, http.MethodGet, "/internal/preferences/"+url.PathEscape(userID),
		httpkit.CallOptions{Attempts: 3}, nil, &out)
	return out, err
}

// Money is an amount in minor units.
type Money struct {
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

// OrderItem is an order line.
type OrderItem struct {
	ProductID    string `json:"product_id"`
	NameSnapshot string `json:"name_snapshot"`
	Quantity     int64  `json:"quantity"`
	LineTotal    Money  `json:"line_total"`
}

// GuestContact is a guest order's contact.
type GuestContact struct {
	Email     string `json:"email"`
	PhoneE164 string `json:"phone_e164"`
}

// Order is what yc-message needs of an order.
type Order struct {
	OrderID          string        `json:"order_id"`
	OrderNumber      string        `json:"order_number"`
	UserID           string        `json:"user_id"`
	UserType         string        `json:"user_type"`
	GuestContact     *GuestContact `json:"guest_contact"`
	RequiresShipping bool          `json:"requires_shipping"`
	Items            []OrderItem   `json:"items"`
	ItemsTotal       Money         `json:"items_total"`
	Shipping         Money         `json:"shipping"`
	TaxTotal         Money         `json:"tax_total"`
	Total            Money         `json:"total"`
	PlacedAt         *time.Time    `json:"placed_at"`
}

// Order returns an order.
func (c *Clients) Order(ctx context.Context, orderID string) (Order, error) {
	var out Order
	err := c.client.Call(ctx, http.MethodGet, "/internal/orders/"+url.PathEscape(orderID),
		httpkit.CallOptions{Attempts: 3}, nil, &out)
	return out, err
}
