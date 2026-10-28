// Package domain holds yc-orders' order snapshot and its status rules.
package domain

import (
	"encoding/json"
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
)

// Status is an order's state: the checkout states, then fulfilment
// (ADR-022) and its ends.
type Status string

// Order states.
const (
	StatusPendingPayment    Status = "pending_payment"
	StatusAbandoned         Status = "abandoned"
	StatusReceived          Status = "received"
	StatusCancelled         Status = "cancelled"
	StatusPackaged          Status = "packaged"
	StatusShipped           Status = "shipped"
	StatusDelivered         Status = "delivered"
	StatusDeliveryException Status = "delivery_exception"
	StatusReturned          Status = "returned"
	StatusDisputed          Status = "disputed"
)

// Placed reports whether the order was paid and confirmed: only placed
// orders are shown to customers and in admin listings.
func (s Status) Placed() bool { return s != StatusPendingPayment && s != StatusAbandoned && s != "" }

// PlacedStatuses are the statuses Placed accepts.
var PlacedStatuses = []Status{
	StatusReceived, StatusCancelled, StatusPackaged, StatusShipped, StatusDelivered, StatusDeliveryException, StatusReturned,
	StatusDisputed,
}

// Open reports whether the order is still in progress: its customer
// cannot be deleted until it finishes (ADR-005). A disputed order waits on
// the bank, not the customer, so it does not count.
func (s Status) Open() bool {
	switch s {
	case StatusPendingPayment, StatusReceived, StatusPackaged, StatusShipped, StatusDeliveryException:
		return true
	case StatusAbandoned, StatusCancelled, StatusDelivered, StatusReturned, StatusDisputed:
	}
	return false
}

// from lists the states each state is reached from. Shipping events are
// consumed independently, so delivered may arrive before in transit:
// delivered is reached from packaged too.
var from = map[Status][]Status{
	StatusReceived:          {StatusPendingPayment},
	StatusAbandoned:         {StatusPendingPayment},
	StatusCancelled:         {StatusReceived, StatusPackaged},
	StatusPackaged:          {StatusReceived},
	StatusShipped:           {StatusPackaged, StatusDeliveryException},
	StatusDelivered:         {StatusPackaged, StatusShipped, StatusDeliveryException},
	StatusDeliveryException: {StatusPackaged, StatusShipped},
	StatusReturned:          {StatusDelivered},
	// A chargeback can arrive at any point after payment (ADR-025).
	StatusDisputed: {
		StatusReceived, StatusCancelled, StatusPackaged, StatusShipped, StatusDelivered, StatusDeliveryException,
		StatusReturned,
	},
}

// CanBecome reports whether an order in s may move to next.
func (s Status) CanBecome(next Status) bool {
	for _, f := range from[next] {
		if f == s {
			return true
		}
	}
	return false
}

// UserType is who placed an order.
type UserType string

// User types.
const (
	UserCustomer UserType = "customer"
	UserVisitor  UserType = "visitor"
)

// GuestContact is how a visitor is reached (ADR-010).
type GuestContact struct {
	Email     string `json:"email"`
	PhoneE164 string `json:"phone_e164"`
}

// Item is one order line as checkout priced it.
type Item struct {
	SKUID          string         `json:"sku_id"`
	ProductID      string         `json:"product_id,omitempty"`
	SKUCode        string         `json:"sku_code"`
	NameSnapshot   string         `json:"name_snapshot"`
	VariantValues  map[string]any `json:"variant_values_snapshot,omitempty"`
	Quantity       int64          `json:"quantity"`
	UnitPriceMinor int64          `json:"unit_price_minor"`
	LineTotalMinor int64          `json:"line_total_minor"`
	TaxMinor       int64          `json:"tax_minor"`
	IsDigital      bool           `json:"is_digital"`
}

// Order is an order snapshot.
type Order struct {
	ID                   string
	OrderNumber          string
	CheckoutSessionID    string
	UserID               string
	UserType             UserType
	GuestContact         *GuestContact
	DeliveryAddress      map[string]any
	DeliveryAddressID    string // optional book entry id (issue #32); empty for guests
	BillingAddress       *BillingAddress
	SellerInvoice        *SellerInvoice
	DeliverySlot         *DeliverySlot
	RequiresShipping     bool
	Status               Status
	Items                []Item
	PricingSnapshot      json.RawMessage
	ShippingSnapshot     json.RawMessage
	Currency             string
	ItemsTotalMinor      int64
	ShippingMinor        int64
	TaxTotalMinor        int64
	TotalMinor           int64
	PaymentTransactionID string
	PaymentGateway       string
	RefundedMinor        int64
	ChargebackID         string
	Shipment             Shipment
	CreatedAt            time.Time
	UpdatedAt            time.Time
	PlacedAt             *time.Time
}

// Shipment is the order's parcel as yc-shipping reports it.
type Shipment struct {
	ID               string
	TrackingNumber   string
	CarrierName      string
	CarrierReference string
	PackagedAt       *time.Time
	ShippedAt        *time.Time
	DeliveredAt      *time.Time
}

// ShippingChoice is the option checkout's customer chose, from the
// shipping snapshot.
type ShippingChoice struct {
	Method string
	RateID string
}

// ShippingChoice reads the method and rate from the shipping snapshot.
// Snapshots from before yc-shipping name the method "id".
func (o Order) ShippingChoice() ShippingChoice {
	var s struct {
		ID     string `json:"id"`
		Method string `json:"method"`
		RateID string `json:"rate_id"`
	}
	_ = json.Unmarshal(o.ShippingSnapshot, &s)
	c := ShippingChoice{Method: s.Method, RateID: s.RateID}
	if c.Method == "" {
		c.Method = s.ID
	}
	if c.Method == "" {
		c.Method = "standard"
	}
	if uuid.Validate(c.RateID) != nil {
		c.RateID = ""
	}
	return c
}

// RefundableMinor is what is left to refund.
func (o Order) RefundableMinor() int64 { return max(o.TotalMinor-o.RefundedMinor, 0) }

// MaxItems bounds an order's lines.
const MaxItems = 100

var (
	currencyRE = regexp.MustCompile(`^[A-Z]{3}$`)
	e164RE     = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)
)

// Validate checks a new order from checkout.
func (o Order) Validate() error {
	v := ycerrors.Validation()
	if uuid.Validate(o.CheckoutSessionID) != nil {
		v.Field("checkout_session_id", "invalid", "Checkout session ID must be a UUID")
	}
	if o.UserID == "" || len(o.UserID) > 100 {
		v.Field("user_id", "required", "User ID is required")
	}
	switch o.UserType {
	case UserCustomer:
	case UserVisitor:
		if o.GuestContact == nil {
			v.Field("guest_contact", "required", "Guest orders need an email and phone (ADR-010)")
		}
	default:
		v.Field("user_type", "invalid", "User type must be customer or visitor")
	}
	if g := o.GuestContact; g != nil {
		if a, err := mail.ParseAddress(g.Email); err != nil || a.Address != g.Email || len(g.Email) > 254 {
			v.Field("guest_contact.email", "invalid", "Email is not valid")
		}
		if !e164RE.MatchString(g.PhoneE164) {
			v.Field("guest_contact.phone_e164", "invalid", "Phone must be in E.164 format")
		}
	}
	if o.RequiresShipping && len(o.DeliveryAddress) == 0 {
		v.Field("delivery_address", "required", "A delivery address is required for physical items")
	}
	if o.BillingAddress != nil {
		if err := ValidateBilling(*o.BillingAddress); err != nil {
			if e, ok := ycerrors.As(err); ok {
				for _, f := range e.Fields {
					v.Field(f.Field, f.Code, f.Message)
				}
			} else {
				v.Field("billing_address", "invalid", "Billing address is not valid")
			}
		}
	}
	if !currencyRE.MatchString(o.Currency) {
		v.Field("currency", "invalid", "Currency must be an ISO 4217 code")
	}
	if len(o.Items) == 0 || len(o.Items) > MaxItems {
		v.Field("items", "invalid", fmt.Sprintf("An order has 1 to %d lines", MaxItems))
	}
	var sum int64
	physical := false
	for i, it := range o.Items {
		f := fmt.Sprintf("items[%d]", i)
		if uuid.Validate(it.SKUID) != nil {
			v.Field(f+".sku_id", "invalid", "SKU ID must be a UUID")
		}
		if it.Quantity <= 0 {
			v.Field(f+".quantity", "invalid", "Quantity must be positive")
		}
		// Tax is rounded per line, so a line total need not be exactly unit
		// price times quantity.
		if it.UnitPriceMinor < 0 || it.LineTotalMinor < 0 || it.TaxMinor < 0 || it.TaxMinor > it.LineTotalMinor {
			v.Field(f+".line_total_minor", "invalid", "Line amounts must not be negative, and tax is part of the total")
		}
		if strings.TrimSpace(it.NameSnapshot) == "" {
			v.Field(f+".name_snapshot", "required", "Name is required")
		}
		sum += it.LineTotalMinor
		physical = physical || !it.IsDigital
	}
	if physical && !o.RequiresShipping {
		v.Field("requires_shipping", "invalid", "Physical items require shipping")
	}
	if o.ItemsTotalMinor != sum {
		v.Field("items_total", "invalid", "Items total must be the sum of the lines")
	}
	if o.ShippingMinor < 0 || o.TaxTotalMinor < 0 || o.TotalMinor < 0 {
		v.Field("total", "invalid", "Amounts must not be negative")
	}
	if len(o.PricingSnapshot) == 0 || !json.Valid(o.PricingSnapshot) {
		v.Field("pricing_snapshot", "required", "The pricing snapshot is required")
	}
	if o.PaymentTransactionID != "" && uuid.Validate(o.PaymentTransactionID) != nil {
		v.Field("payment.transaction_id", "invalid", "Transaction ID must be a UUID")
	}
	if v.HasErrors() {
		return v.Build()
	}
	return nil
}

// SourceEmail is the email a guest order can later be claimed by.
func (o Order) SourceEmail() string {
	if o.GuestContact == nil {
		return ""
	}
	return strings.ToLower(o.GuestContact.Email)
}

// Transition moves the order to next, or reports why it cannot. A move
// to the state it is already in is allowed (repeated calls succeed).
func (o *Order) Transition(next Status, now time.Time) error {
	if o.Status == next {
		return nil
	}
	if !o.Status.CanBecome(next) {
		return ycerrors.New(ycerrors.CodeInvalidStateTransition,
			fmt.Sprintf("An order that is %s cannot become %s", o.Status, next)).
			With("order_status", string(o.Status))
	}
	if next == StatusPackaged && !o.RequiresShipping {
		return ycerrors.New(ycerrors.CodeInvalidStateTransition, "A digital order is not shipped").
			With("order_status", string(o.Status))
	}
	o.Status = next
	o.UpdatedAt = now
	switch next {
	case StatusReceived:
		o.PlacedAt = &now
	case StatusPackaged:
		o.Shipment.PackagedAt = &now
	case StatusShipped:
		o.Shipment.ShippedAt = &now
	case StatusDelivered:
		o.Shipment.DeliveredAt = &now
	case StatusPendingPayment, StatusAbandoned, StatusCancelled, StatusDeliveryException, StatusReturned, StatusDisputed:
	}
	return nil
}

// Pseudonymise replaces the buyer's identity with pseudonym and clears
// how they were reached; amounts and lines stay for the books (ADR-005).
func (o *Order) Pseudonymise(pseudonym string) {
	o.UserID = pseudonym
	o.GuestContact = nil
	o.DeliveryAddress = nil
	o.BillingAddress = nil
}

// OrderNumber formats a shop's nth order number, e.g. DEMO-SHOP-000042.
func OrderNumber(tenantID string, n int64) string {
	return fmt.Sprintf("%s-%06d", strings.ToUpper(tenantID), n)
}

// HistoryEntry is one status change.
type HistoryEntry struct {
	From      Status
	To        Status
	ActorID   string
	Reason    string
	CreatedAt time.Time
}
