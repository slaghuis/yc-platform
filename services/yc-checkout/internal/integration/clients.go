// Package integration holds yc-checkout's clients for the saga's
// participants. Every call goes through yc-gateway's internal listener
// with yc-checkout's service token (ADR-019 rule 1, ADR-021).
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	money "github.com/slaghuis/yc-platform/libs/yc-money"
)

// Caller makes internal calls (httpkit.InternalClient).
type Caller interface {
	Call(ctx context.Context, method, path string, opts httpkit.CallOptions, in, out any) error
}

// Timeouts per attempt (ADR-019 rule 6): payment calls outlast the
// gateway's own 30 s timeout.
type Timeouts struct {
	Default  time.Duration
	Payment  time.Duration
	Attempts int
}

// Rejection is a participant's 4xx answer: a business decision, not
// retried (ADR-019 rule 6).
type Rejection struct {
	Status  int
	Problem ycerrors.Problem
	err     error
}

func (r *Rejection) Error() string { return r.err.Error() }
func (r *Rejection) Unwrap() error { return r.err }

// Rejected returns the rejection in err's chain. Any other error is an
// unknown outcome: the participant may or may not have acted.
func Rejected(err error) (*Rejection, bool) {
	var r *Rejection
	ok := errors.As(err, &r)
	return r, ok
}

func classify(err error) error {
	status := httpkit.UpstreamStatus(err)
	if status < 400 || status >= 500 {
		return err
	}
	p, _ := httpkit.UpstreamProblem(err)
	return &Rejection{Status: status, Problem: p, err: err}
}

// Clients calls every participant.
type Clients struct {
	c Caller
	t Timeouts
}

// New returns the participant clients.
func New(c Caller, t Timeouts) *Clients {
	if t.Attempts < 1 {
		t.Attempts = 3
	}
	return &Clients{c: c, t: t}
}

func (c *Clients) call(ctx context.Context, method, path, key string, timeout time.Duration, in, out any) error {
	if timeout == 0 {
		timeout = c.t.Default
	}
	return classify(c.c.Call(ctx, method, path,
		httpkit.CallOptions{IdempotencyKey: key, Timeout: timeout, Attempts: c.t.Attempts}, in, out))
}

// CartLine is one cart line.
type CartLine struct {
	SKUID    string `json:"sku_id"`
	Quantity int64  `json:"quantity"`
}

// Cart is yc-cart's view of a user's cart.
type Cart struct {
	CartID      string     `json:"cart_id"`
	Convertible bool       `json:"convertible"`
	Items       []CartLine `json:"items"`
}

// Cart reads a user's cart (yc-cart).
func (c *Clients) Cart(ctx context.Context, userID, cartType string) (Cart, error) {
	var out Cart
	err := c.call(ctx, http.MethodGet, "/internal/carts/"+url.PathEscape(userID)+"/"+url.PathEscape(cartType), "", 0, nil, &out)
	return out, err
}

// SKU is yc-product's description of a SKU.
type SKU struct {
	SKUID            string         `json:"sku_id"`
	ProductID        string         `json:"product_id"`
	SKUCode          string         `json:"sku_code"`
	ProductName      string         `json:"product_name"`
	VariantValues    map[string]any `json:"variant_values"`
	IsDigital        bool           `json:"is_digital"`
	InventoryTracked bool           `json:"inventory_tracked"`
	TaxClass         string         `json:"tax_class"`
	WeightGrams      *int64         `json:"weight_grams,omitempty"`
	Status           string         `json:"status"`
}

// SKUBatch is the SKUs found and the IDs that were not.
type SKUBatch struct {
	SKUs    []SKU    `json:"skus"`
	Missing []string `json:"missing"`
}

// SKUs looks up SKUs (yc-product).
func (c *Clients) SKUs(ctx context.Context, ids []string) (SKUBatch, error) {
	var out SKUBatch
	q := url.Values{"ids": {strings.Join(ids, ",")}}
	err := c.call(ctx, http.MethodGet, "/internal/skus/batch?"+q.Encode(), "", 0, nil, &out)
	return out, err
}

// PricedLine is yc-pricing's price for one line.
type PricedLine struct {
	SKUID    string      `json:"sku_id"`
	Quantity int64       `json:"quantity"`
	Net      money.Money `json:"net"`
	Tax      money.Money `json:"tax"`
	Gross    money.Money `json:"gross"`
}

// Unpriced is a line yc-pricing could not price.
type Unpriced struct {
	SKUID  string `json:"sku_id"`
	Reason string `json:"reason"`
}

// Pricing is yc-pricing's evaluation. Raw is the full result, kept as the
// session's pricing snapshot.
type Pricing struct {
	StoreCurrency string       `json:"store_currency"`
	TaxInclusive  bool         `json:"tax_inclusive"`
	Lines         []PricedLine `json:"lines"`
	Unpriced      []Unpriced   `json:"unpriced"`
	Totals        struct {
		TaxTotal   money.Money `json:"tax_total"`
		GrandTotal money.Money `json:"grand_total"`
	} `json:"totals"`
	Raw json.RawMessage `json:"-"`
}

// Evaluate prices the lines in the store currency (yc-pricing).
func (c *Clients) Evaluate(ctx context.Context, lines []CartLine) (Pricing, error) {
	var raw json.RawMessage
	if err := c.call(ctx, http.MethodPost, "/internal/prices/evaluate", "", 0,
		map[string]any{"lines": lines}, &raw); err != nil {
		return Pricing{}, err
	}
	var out Pricing
	if err := json.Unmarshal(raw, &out); err != nil {
		return Pricing{}, err
	}
	out.Raw = raw
	return out, nil
}

// Reservation is yc-inventory's hold on the tracked lines. ID is empty
// when nothing needed stock.
type Reservation struct {
	ID        string     `json:"reservation_id"`
	ExpiresAt *time.Time `json:"expires_at"`
}

// Shortage is a line inventory cannot hold.
type Shortage struct {
	SKUID     string `json:"sku_id"`
	Requested int64  `json:"requested"`
	Available int64  `json:"available"`
}

// Reserve holds stock for the session (yc-inventory).
func (c *Clients) Reserve(ctx context.Context, key, sessionID string, lines []CartLine, ttlMinutes int) (Reservation, error) {
	var out Reservation
	err := c.call(ctx, http.MethodPost, "/internal/reservations", key, 0,
		map[string]any{"session_id": sessionID, "items": lines, "ttl_minutes": ttlMinutes}, &out)
	return out, err
}

// Commit takes the reserved stock out of on-hand (yc-inventory).
func (c *Clients) Commit(ctx context.Context, key, reservationID, reference string) error {
	return c.call(ctx, http.MethodPost, "/internal/reservations/"+url.PathEscape(reservationID)+"/commit", key, 0,
		map[string]string{"reference": reference}, nil)
}

// Release returns the reserved stock (yc-inventory). Already released,
// expired or unknown reservations succeed.
func (c *Clients) Release(ctx context.Context, key, reservationID string) error {
	return c.call(ctx, http.MethodPost, "/internal/reservations/"+url.PathEscape(reservationID)+"/release", key, 0, nil, nil)
}

// Payment statuses yc-payment reports.
const (
	PaymentCreated    = "created"
	PaymentPending    = "pending"
	PaymentAuthorized = "authorized"
	PaymentCaptured   = "captured"
	PaymentVoided     = "voided"
	PaymentRefunded   = "refunded"
	PaymentFailed     = "failed"
)

// Transaction is yc-payment's view of a payment.
type Transaction struct {
	ID            string         `json:"transaction_id"`
	Status        string         `json:"status"`
	NextAction    map[string]any `json:"next_action"`
	FailureReason string         `json:"failure_reason"`
}

// PaymentRequest asks for an authorization (Variant A) or a charge
// (Variant B) of the session's total. A hosted gateway takes no token.
type PaymentRequest struct {
	SessionID    string      `json:"session_id"`
	Gateway      string      `json:"gateway,omitempty"`
	Amount       money.Money `json:"amount"`
	PaymentToken string      `json:"payment_token,omitempty"`
	ReturnURL    string      `json:"return_url,omitempty"`
	OrderID      string      `json:"order_id,omitempty"`
}

// Authorize places a hold on the customer's payment method (yc-payment).
// Repeating key returns the transaction's current status.
func (c *Clients) Authorize(ctx context.Context, key string, req PaymentRequest) (Transaction, error) {
	var out Transaction
	err := c.call(ctx, http.MethodPost, "/internal/authorize", key, c.t.Payment, req, &out)
	return out, err
}

// Charge takes the payment in one step (yc-payment). For a hosted gateway
// the answer is pending with a redirect; repeating key returns the
// transaction's current status.
func (c *Clients) Charge(ctx context.Context, key string, req PaymentRequest) (Transaction, error) {
	var out Transaction
	err := c.call(ctx, http.MethodPost, "/internal/charge", key, c.t.Payment, req, &out)
	return out, err
}

// Route is the shop's gateway for a currency and what it can do.
type Route struct {
	Gateway                     string `json:"gateway"`
	SupportsSeparateAuthCapture bool   `json:"supports_separate_auth_capture"`
	HostedPayment               bool   `json:"hosted_payment"`
	PublicKey                   string `json:"public_key"`
}

// Route picks the shop's gateway for currency, or checks the named one
// (yc-payment). A shop that cannot take the payment is 412.
func (c *Clients) Route(ctx context.Context, currency, gateway string) (Route, error) {
	q := url.Values{"currency": {currency}}
	if gateway != "" {
		q.Set("gateway", gateway)
	}
	var out Route
	err := c.call(ctx, http.MethodGet, "/internal/gateway-route?"+q.Encode(), "", 0, nil, &out)
	return out, err
}

// Refund statuses yc-payment reports.
const (
	RefundPending   = "pending"
	RefundCompleted = "completed"
	RefundFailed    = "failed"
	RefundNone      = "none"
)

// RefundResult is a refund's state; status none: no such transaction.
type RefundResult struct {
	Status string `json:"status"`
	Refund *struct {
		ID            string `json:"refund_id"`
		AmountMinor   int64  `json:"amount_minor"`
		FailureReason string `json:"failure_reason"`
	} `json:"refund"`
}

// Refund returns what is left of a captured payment (yc-payment).
func (c *Clients) Refund(ctx context.Context, key, transactionID, orderID, reason string) (RefundResult, error) {
	in := map[string]string{"transaction_id": transactionID, "reason": reason}
	if orderID != "" {
		in["order_id"] = orderID
	}
	var out RefundResult
	err := c.call(ctx, http.MethodPost, "/internal/refund", key, c.t.Payment, in, &out)
	return out, err
}

// Capture takes the authorized amount (yc-payment).
func (c *Clients) Capture(ctx context.Context, key, transactionID, orderID string) (Transaction, error) {
	var out Transaction
	err := c.call(ctx, http.MethodPost, "/internal/capture", key, c.t.Payment,
		map[string]string{"transaction_id": transactionID, "order_id": orderID}, &out)
	return out, err
}

// Void releases an authorization, found by its transaction or by the
// authorize call's key. Nothing to void succeeds with status none.
func (c *Clients) Void(ctx context.Context, key, transactionID, authorizationKey string) (string, error) {
	in := map[string]string{"authorization_key": authorizationKey}
	if transactionID != "" {
		in = map[string]string{"transaction_id": transactionID}
	}
	var out struct {
		Status string `json:"status"`
	}
	err := c.call(ctx, http.MethodPost, "/internal/void", key, c.t.Payment, in, &out)
	return out.Status, err
}

// Transaction reads a payment (yc-payment).
func (c *Clients) Transaction(ctx context.Context, id string) (Transaction, error) {
	var out Transaction
	err := c.call(ctx, http.MethodGet, "/internal/transactions/"+url.PathEscape(id), "", 0, nil, &out)
	return out, err
}

// OrderItem is one order line.
type OrderItem struct {
	SKUID                 string         `json:"sku_id"`
	ProductID             string         `json:"product_id,omitempty"`
	SKUCode               string         `json:"sku_code,omitempty"`
	NameSnapshot          string         `json:"name_snapshot"`
	VariantValuesSnapshot map[string]any `json:"variant_values_snapshot,omitempty"`
	Quantity              int64          `json:"quantity"`
	UnitPriceMinor        int64          `json:"unit_price_minor"`
	LineTotalMinor        int64          `json:"line_total_minor"`
	TaxMinor              int64          `json:"tax_minor"`
	IsDigital             bool           `json:"is_digital"`
}

// GuestContact is a visitor's contact details.
type GuestContact struct {
	Email     string `json:"email"`
	PhoneE164 string `json:"phone_e164"`
}

// OrderPayment names the payment: the authorization (Variant A), or only
// the gateway when the charge follows the order (Variant B).
type OrderPayment struct {
	TransactionID string `json:"transaction_id,omitempty"`
	Gateway       string `json:"gateway"`
}

// BillingAddress is structured SA billing for the order invoice (issue #29).
type BillingAddress struct {
	CompanyName     string `json:"company_name,omitempty"`
	VATNumber       string `json:"vat_number,omitempty"`
	StreetAndNumber string `json:"street_and_number"`
	Suburb          string `json:"suburb,omitempty"`
	TownCity        string `json:"town_city"`
	PostalCode      string `json:"postal_code"`
	CountryCode     string `json:"country_code,omitempty"`
}

// CreateOrder is the pending_payment order's snapshot.
type CreateOrder struct {
	CheckoutSessionID string          `json:"checkout_session_id"`
	UserID            string          `json:"user_id"`
	UserType          string          `json:"user_type"`
	GuestContact      *GuestContact   `json:"guest_contact,omitempty"`
	DeliveryAddress   map[string]any  `json:"delivery_address,omitempty"`
	DeliveryAddressID string          `json:"delivery_address_id,omitempty"`
	BillingAddress    *BillingAddress `json:"billing_address,omitempty"`
	RequiresShipping  bool            `json:"requires_shipping"`
	Items             []OrderItem     `json:"items"`
	Currency          string          `json:"currency"`
	ItemsTotalMinor   int64           `json:"items_total_minor"`
	ShippingMinor     int64           `json:"shipping_minor"`
	TaxTotalMinor     int64           `json:"tax_total_minor"`
	TotalMinor        int64           `json:"total_minor"`
	PricingSnapshot   json.RawMessage `json:"pricing_snapshot,omitempty"`
	ShippingOption    map[string]any  `json:"shipping_option,omitempty"`
	DeliverySlot      *DeliverySlot   `json:"delivery_slot,omitempty"`
	Payment           *OrderPayment   `json:"payment,omitempty"`
}

// DeliveryAddress is one yc-identity book entry (issue #32).
type DeliveryAddress struct {
	ID        string         `json:"id"`
	Label     string         `json:"label,omitempty"`
	Address   map[string]any `json:"address"`
	IsDefault bool           `json:"is_default"`
}

// DeliverySlot is the chosen window snapshotted onto the order (issue #30).
type DeliverySlot struct {
	SlotID     string    `json:"slot_id"`
	StartAt    time.Time `json:"start_at"`
	EndAt      time.Time `json:"end_at"`
	Timezone   string    `json:"timezone"`
	Label      string    `json:"label,omitempty"`
	ScheduleID string    `json:"schedule_id,omitempty"`
}

// Order is yc-orders' view of an order.
type Order struct {
	ID          string `json:"order_id"`
	OrderNumber string `json:"order_number"`
	Status      string `json:"status"`
}

// CreateOrder records a pending_payment order (yc-orders). A session has
// one order: repeating returns it.
func (c *Clients) CreateOrder(ctx context.Context, key string, in CreateOrder) (Order, error) {
	var out Order
	err := c.call(ctx, http.MethodPost, "/internal/orders", key, 0, in, &out)
	return out, err
}

// ConfirmOrder places the paid order (yc-orders).
func (c *Clients) ConfirmOrder(ctx context.Context, key, orderID, transactionID string) (Order, error) {
	var out Order
	err := c.call(ctx, http.MethodPost, "/internal/orders/"+url.PathEscape(orderID)+"/confirm", key, 0,
		map[string]string{"payment_transaction_id": transactionID}, &out)
	return out, err
}

// ShippingQuoteItem is one line to ship.
type ShippingQuoteItem struct {
	SKUID       string `json:"sku_id"`
	Quantity    int64  `json:"quantity"`
	WeightGrams *int64 `json:"weight_grams,omitempty"`
	IsDigital   bool   `json:"is_digital,omitempty"`
}

// ShippingQuoteRequest asks what shipping the items to an address costs.
type ShippingQuoteRequest struct {
	DeliveryAddress map[string]any      `json:"delivery_address"`
	Items           []ShippingQuoteItem `json:"items"`
	SubtotalMinor   int64               `json:"subtotal_minor"`
}

// ShippingQuoteOption is one way to ship, priced.
type ShippingQuoteOption struct {
	OptionID             string `json:"option_id"`
	RateID               string `json:"rate_id"`
	Name                 string `json:"name"`
	AmountMinor          int64  `json:"amount_minor"`
	EstimatedDaysMin     *int   `json:"estimated_days_min,omitempty"`
	EstimatedDaysMax     *int   `json:"estimated_days_max,omitempty"`
	ScheduleID           string `json:"schedule_id,omitempty"`
	RequiresDeliverySlot bool   `json:"requires_delivery_slot,omitempty"`
}

// Slot is one delivery window with remaining capacity (yc-shipping).
type Slot struct {
	SlotID            string    `json:"slot_id"`
	StartAt           time.Time `json:"start_at"`
	EndAt             time.Time `json:"end_at"`
	Timezone          string    `json:"timezone"`
	RemainingCapacity int       `json:"remaining_capacity"`
	Label             string    `json:"label,omitempty"`
	ScheduleID        string    `json:"schedule_id,omitempty"`
}

// SlotList is yc-shipping's available windows for a rate.
type SlotList struct {
	Slots []Slot `json:"slots"`
}

// ShippingQuote is yc-shipping's options for an address, cheapest first.
// None means the shop does not ship there.
type ShippingQuote struct {
	Options []ShippingQuoteOption `json:"options"`
}

// QuoteShipping prices shipping to an address (yc-shipping).
func (c *Clients) QuoteShipping(ctx context.Context, req ShippingQuoteRequest) (ShippingQuote, error) {
	var out ShippingQuote
	err := c.call(ctx, http.MethodPost, "/internal/quote", "", 0, req, &out)
	return out, err
}

// GetDeliveryAddress loads an active book entry (yc-identity).
func (c *Clients) GetDeliveryAddress(ctx context.Context, userID, addressID string) (DeliveryAddress, error) {
	var out DeliveryAddress
	err := c.call(ctx, http.MethodGet,
		"/internal/users/"+url.PathEscape(userID)+"/addresses/"+url.PathEscape(addressID), "", 0, nil, &out)
	return out, err
}

// CreateDeliveryAddress saves a book entry for the customer (yc-identity).
func (c *Clients) CreateDeliveryAddress(ctx context.Context, userID string, address map[string]any) (DeliveryAddress, error) {
	var out DeliveryAddress
	err := c.call(ctx, http.MethodPost, "/internal/users/"+url.PathEscape(userID)+"/addresses", "", 0,
		map[string]any{"address": address}, &out)
	return out, err
}

// ListSlots returns available delivery windows for a rate (yc-shipping).
func (c *Clients) ListSlots(ctx context.Context, rateID, from, to string) (SlotList, error) {
	q := url.Values{"rate_id": {rateID}}
	if from != "" {
		q.Set("from", from)
	}
	if to != "" {
		q.Set("to", to)
	}
	var out SlotList
	err := c.call(ctx, http.MethodGet, "/internal/slots?"+q.Encode(), "", 0, nil, &out)
	return out, err
}

// GetSlot returns one window when it still has capacity (yc-shipping).
func (c *Clients) GetSlot(ctx context.Context, slotID string) (Slot, error) {
	var out Slot
	err := c.call(ctx, http.MethodGet, "/internal/slots/"+url.PathEscape(slotID), "", 0, nil, &out)
	return out, err
}

// BookSlot reserves capacity for bookingKey (checkout session id).
func (c *Clients) BookSlot(ctx context.Context, slotID, bookingKey string) error {
	return c.call(ctx, http.MethodPost, "/internal/slots/"+url.PathEscape(slotID)+"/book", "", 0,
		map[string]string{"booking_key": bookingKey}, nil)
}

// ReleaseSlot frees a booking keyed by bookingKey (idempotent).
func (c *Clients) ReleaseSlot(ctx context.Context, slotID, bookingKey string) error {
	path := "/internal/slots/release"
	if slotID != "" {
		path = "/internal/slots/" + url.PathEscape(slotID) + "/release"
	}
	return c.call(ctx, http.MethodPost, path, "", 0, map[string]string{"booking_key": bookingKey}, nil)
}

// AbandonOrder closes a pending order (yc-orders). No order is nothing to
// undo.
func (c *Clients) AbandonOrder(ctx context.Context, key, orderID, reason string) error {
	return c.call(ctx, http.MethodPost, "/internal/orders/"+url.PathEscape(orderID)+"/abandon", key, 0,
		map[string]string{"reason": reason}, nil)
}
