// Package domain holds the checkout session and its saga state machine
// (ADR-019): Variant A (authorize, then capture) and Variant B (one
// combined charge, ADR-013).
package domain

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/mail"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
)

// State is a session's saga state.
type State string

// States. PRICING holds the quote until the customer confirms.
const (
	StatePricing      State = "PRICING"
	StateReserving    State = "RESERVING"
	StateAuthorizing  State = "AUTHORIZING"
	StateOrderCreate  State = "ORDER_CREATE"
	StateCharging     State = "CHARGING"
	StateCapturing    State = "CAPTURING"
	StateConfirming   State = "CONFIRMING"
	StateCompleted    State = "COMPLETED"
	StateCompensating State = "COMPENSATING"
	StateFailed       State = "FAILED"
	StateExpired      State = "EXPIRED"
)

// States lists every state, in saga order.
var States = []State{
	StatePricing, StateReserving, StateAuthorizing, StateOrderCreate, StateCharging, StateCapturing,
	StateConfirming, StateCompleted, StateCompensating, StateFailed, StateExpired,
}

// Variant is how a session pays (ADR-013): the gateway's capabilities
// decide it.
type Variant string

// Variants.
const (
	// VariantAuthCapture authorises before the order is created and
	// captures after (Variant A).
	VariantAuthCapture Variant = "auth_capture"
	// VariantCharge charges once, after the order is created (Variant B).
	VariantCharge Variant = "charge"
)

// steps are each variant's saga steps, in order (ADR-019 rule 2).
var steps = map[Variant][]State{
	VariantAuthCapture: {StateReserving, StateAuthorizing, StateOrderCreate, StateCapturing, StateConfirming, StateCompleted},
	VariantCharge:      {StateReserving, StateOrderCreate, StateCharging, StateConfirming, StateCompleted},
}

// Routing is the gateway a session pays with and what it can do.
type Routing struct {
	Gateway   string
	Variant   Variant
	Hosted    bool
	PublicKey string
}

// Valid reports whether s is a known state.
func (s State) Valid() bool { return slices.Contains(States, s) }

// Terminal reports whether the session is over.
func (s State) Terminal() bool {
	return s == StateCompleted || s == StateFailed || s == StateExpired
}

// Driven reports whether the saga works on the session without waiting for
// the customer.
func (s State) Driven() bool {
	switch s {
	case StateReserving, StateAuthorizing, StateOrderCreate, StateCharging, StateCapturing, StateConfirming, StateCompensating:
		return true
	case StatePricing, StateCompleted, StateFailed, StateExpired:
	}
	return false
}

// Compensable reports whether the session is short of the point of no
// return (capture or charge succeeded), so a failure or expiry is
// compensated.
func (s State) Compensable() bool {
	switch s {
	case StatePricing, StateReserving, StateAuthorizing, StateOrderCreate, StateCharging, StateCapturing:
		return true
	case StateConfirming, StateCompleted, StateCompensating, StateFailed, StateExpired:
	}
	return false
}

// UserType is who checks out: a registered customer or a guest visitor.
type UserType string

// User types.
const (
	UserCustomer UserType = "customer"
	UserVisitor  UserType = "visitor"
)

// Failure reasons (checkout.failed.v1, checkout.compensating.v1).
const (
	ReasonInsufficientStock  = "insufficient_stock"
	ReasonPaymentDeclined    = "payment_declined"
	ReasonCaptureFailed      = "capture_failed"
	ReasonReservationExpired = "reservation_expired"
	ReasonSessionExpired     = "session_expired"
	ReasonCancelled          = "cancelled"
	ReasonStepFailed         = "step_failed"
)

// ComponentSnapshot is one kit component as yc-product defined it for a
// single bundle unit (composed-offers FR). Multiply Quantity by the line
// quantity when reserving stock.
type ComponentSnapshot struct {
	SKUID       string `json:"sku_id"`
	Quantity    int64  `json:"quantity"`
	ProductName string `json:"product_name,omitempty"`
	SKUCode     string `json:"sku_code,omitempty"`
}

// Item is one snapshotted cart line with its authoritative price.
type Item struct {
	SKUID            string         `json:"sku_id"`
	ProductID        string         `json:"product_id"`
	SKUCode          string         `json:"sku_code"`
	Name             string         `json:"name"`
	VariantValues    map[string]any `json:"variant_values,omitempty"`
	Quantity         int64          `json:"quantity"`
	IsDigital        bool           `json:"is_digital"`
	InventoryTracked bool           `json:"inventory_tracked"`
	TaxClass         string         `json:"tax_class"`
	// WeightGrams is the SKU's weight, when the shop recorded it.
	WeightGrams *int64 `json:"weight_grams,omitempty"`
	// UnitPriceMinor is gross per unit, rounded; LineTotalMinor is the
	// line's gross, including TaxMinor.
	UnitPriceMinor int64 `json:"unit_price_minor"`
	LineTotalMinor int64 `json:"line_total_minor"`
	TaxMinor       int64 `json:"tax_minor"`
	// Components is the kit expansion for a bundle SKU (per one unit).
	// Empty for ordinary lines. Persisted for packing lists.
	Components []ComponentSnapshot `json:"components,omitempty"`
}

// ShippingOption is a way to deliver the physical items, as yc-shipping
// priced it for the delivery address. ID is the rate's method.
type ShippingOption struct {
	ID                   string `json:"id"`
	RateID               string `json:"rate_id,omitempty"`
	Name                 string `json:"name"`
	AmountMinor          int64  `json:"amount_minor"`
	EstimatedDaysMin     *int   `json:"estimated_days_min,omitempty"`
	EstimatedDaysMax     *int   `json:"estimated_days_max,omitempty"`
	ScheduleID           string `json:"schedule_id,omitempty"`
	RequiresDeliverySlot bool   `json:"requires_delivery_slot,omitempty"`
}

// ShippingChoice is the shipping step: a delivery address to quote, an
// option, or both. DeliveryAddressID selects a customer book entry (issue #32).
type ShippingChoice struct {
	DeliveryAddress   map[string]any
	DeliveryAddressID string
	OptionID          string
}

// GuestContact is a visitor's contact details (ADR-010).
type GuestContact struct {
	Email     string `json:"email"`
	PhoneE164 string `json:"phone_e164"`
}

// Compensation steps. UndoLateCharge refunds a charge that settled after
// the session gave up on it (a hosted payment completed late). UndoSlot
// releases a delivery window booked at ORDER_CREATE (issue #30).
const (
	UndoPayment     = "payment_voided"
	UndoOrder       = "order_abandoned"
	UndoReservation = "reservation_released"
	UndoLateCharge  = "late_charge_refunded"
	UndoSlot        = "slot_released"
)

// CompensationEntry records one finished compensation step.
type CompensationEntry struct {
	Step   string    `json:"step"`
	Result string    `json:"result"`
	At     time.Time `json:"at"`
}

// Session is a checkout session.
type Session struct {
	ID                string
	UserID            string
	UserType          UserType
	CartType          string
	CartID            string
	State             State
	Items             []Item
	RequiresShipping  bool
	HasTrackedItems   bool
	Currency          string
	TaxInclusive      bool
	ItemsTotalMinor   int64
	TaxTotalMinor     int64
	ShippingMinor     int64
	TotalMinor        int64
	PricingSnapshot   json.RawMessage
	ShippingOptions   []ShippingOption
	ShippingOptionID  string
	GuestContact      *GuestContact
	DeliveryAddress   map[string]any
	DeliveryAddressID string // book entry id (issue #32); empty for guests / one-off until saved
	BillingAddress    *BillingAddress
	DeliverySlot      *DeliverySlot

	// PaymentGateway and the rest of the routing are chosen when the
	// session starts and may change at confirm.
	PaymentGateway   string
	PaymentVariant   Variant
	HostedPayment    bool
	PaymentPublicKey string

	ConfirmKey   string
	PaymentToken string
	ReturnURL    string

	ReservationID        string
	ReservationExpiresAt *time.Time
	PaymentTransactionID string
	PaymentStatus        string
	NextAction           map[string]any
	OrderID              string
	OrderNumber          string

	FailedState     State
	FailureReason   string
	FailureDetail   map[string]any
	EndState        State
	CompensationLog []CompensationEntry
	Attempts        int
	NextAttemptAt   *time.Time

	// LeaseOwner drives the session until LeaseUntil.
	LeaseOwner string
	LeaseUntil *time.Time

	ExpiresAt   time.Time
	ConfirmedAt *time.Time
	CompletedAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Owns reports whether the actor is the session's user.
func (s Session) Owns(actorID string, actorType UserType) bool {
	return s.UserID == actorID && s.UserType == actorType
}

// Leased reports whether another driver holds the session.
func (s Session) Leased(now time.Time) bool {
	return s.LeaseOwner != "" && s.LeaseUntil != nil && now.Before(*s.LeaseUntil)
}

// Expired reports whether the quote is no longer honoured (ADR-011).
func (s Session) Expired(now time.Time) bool { return !now.Before(s.ExpiresAt) }

// SelectShipping chooses one of the session's options and updates the
// totals. Shipping is untaxed. Changing the option clears a bound slot.
func (s *Session) SelectShipping(id string) bool {
	for _, o := range s.ShippingOptions {
		if o.ID == id {
			if s.ShippingOptionID != id {
				s.DeliverySlot = nil
			}
			s.ShippingOptionID = id
			s.ShippingMinor = o.AmountMinor
			s.TotalMinor = s.ItemsTotalMinor + s.ShippingMinor
			return true
		}
	}
	return false
}

// ShipTo records the options yc-shipping quoted for an address and
// selects optionID, or the cheapest when it is empty. A previous address's
// options and choice are replaced. It reports whether optionID is one of
// the options. A new address or option clears any bound delivery slot
// (issue #30).
func (s *Session) ShipTo(address map[string]any, options []ShippingOption, optionID, addressID string) bool {
	s.DeliveryAddress, s.DeliveryAddressID, s.ShippingOptions = address, addressID, options
	s.ShippingOptionID, s.ShippingMinor, s.TotalMinor = "", 0, s.ItemsTotalMinor
	s.DeliverySlot = nil
	if optionID == "" && len(options) > 0 {
		optionID = slices.MinFunc(options, func(a, b ShippingOption) int { return cmp.Compare(a.AmountMinor, b.AmountMinor) }).ID
	}
	return s.SelectShipping(optionID)
}

// AddressDiffers reports whether address is not the one shipping was
// quoted for. Keys order and whitespace in values do not matter.
func (s Session) AddressDiffers(address map[string]any) bool {
	return !reflect.DeepEqual(normaliseAddress(address), normaliseAddress(s.DeliveryAddress))
}

func normaliseAddress(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = normaliseAddress(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = normaliseAddress(e)
		}
		return out
	case []string:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = strings.TrimSpace(e)
		}
		return out
	case string:
		return strings.TrimSpace(x)
	default:
		b, _ := json.Marshal(x)
		var out any
		_ = json.Unmarshal(b, &out)
		return out
	}
}

// SelectedShipping returns the chosen option.
func (s Session) SelectedShipping() (ShippingOption, bool) {
	for _, o := range s.ShippingOptions {
		if o.ID == s.ShippingOptionID {
			return o, true
		}
	}
	return ShippingOption{}, false
}

// TrackedItems are the lines inventory reserves. Bundle lines expand to
// their components (qty × line qty); the bundle SKU itself is never
// reserved. Duplicate SKUs across lines are merged.
func (s Session) TrackedItems() []Item {
	qty := map[string]int64{}
	var order []string
	add := func(sku string, n int64) {
		if _, ok := qty[sku]; !ok {
			order = append(order, sku)
		}
		qty[sku] += n
	}
	for _, it := range s.Items {
		if len(it.Components) > 0 {
			for _, c := range it.Components {
				add(c.SKUID, c.Quantity*it.Quantity)
			}
			continue
		}
		if it.InventoryTracked {
			add(it.SKUID, it.Quantity)
		}
	}
	out := make([]Item, 0, len(order))
	for _, id := range order {
		out = append(out, Item{SKUID: id, Quantity: qty[id], InventoryTracked: true})
	}
	return out
}

// AttributeShortages remaps inventory shortages for component SKUs onto
// the bundle line that owns them, so the customer sees a stock conflict
// on the payable bundle SKU (composed-offers FR).
func (s Session) AttributeShortages(raw any) any {
	owner := map[string]string{}
	for _, it := range s.Items {
		for _, c := range it.Components {
			owner[c.SKUID] = it.SKUID
		}
	}
	if len(owner) == 0 {
		return raw
	}
	list, ok := raw.([]any)
	if !ok {
		return raw
	}
	out := make([]any, len(list))
	for i, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			out[i] = e
			continue
		}
		sku, _ := m["sku_id"].(string)
		bundle, mapped := owner[sku]
		if !mapped {
			out[i] = e
			continue
		}
		cp := make(map[string]any, len(m))
		for k, v := range m {
			cp[k] = v
		}
		cp["sku_id"] = bundle
		out[i] = cp
	}
	return out
}

// Key is the Idempotency-Key of a saga call: {session_id}:{step}
// (ADR-019 rule 5).
func (s Session) Key(step string) string { return s.ID + ":" + step }

// UndoKey is the Idempotency-Key of a compensation call.
func (s Session) UndoKey(step string) string { return s.Key(step) + ":undo" }

// Route records the gateway the session pays with.
func (s *Session) Route(r Routing) {
	s.PaymentGateway, s.PaymentVariant, s.HostedPayment, s.PaymentPublicKey = r.Gateway, r.Variant, r.Hosted, r.PublicKey
}

// Variant is how the session pays; sessions from before routing
// authorize and capture.
func (s Session) Variant() Variant {
	if s.PaymentVariant == VariantCharge {
		return VariantCharge
	}
	return VariantAuthCapture
}

// PaymentState is the step that takes the payment: AUTHORIZING or
// CHARGING.
func (s Session) PaymentState() State {
	if s.Variant() == VariantCharge {
		return StateCharging
	}
	return StateAuthorizing
}

// PaymentCall names the payment step's call in its Idempotency-Key.
func (s Session) PaymentCall() string {
	if s.Variant() == VariantCharge {
		return "charge"
	}
	return "authorize"
}

// FirstStep is where a confirmed session starts: RESERVING is skipped when
// no SKU is tracked (ADR-012).
func (s Session) FirstStep() State {
	if s.HasTrackedItems {
		return StateReserving
	}
	return steps[s.Variant()][1]
}

// Next is the state after the current step succeeds.
func (s Session) Next() State {
	order := steps[s.Variant()]
	if i := slices.Index(order, s.State); i >= 0 && i+1 < len(order) {
		return order[i+1]
	}
	return s.State
}

// Reached reports whether the saga got as far as step before failing.
func (s Session) Reached(step State) bool {
	order := steps[s.Variant()]
	at, want := slices.Index(order, s.FailedState), slices.Index(order, step)
	return at >= 0 && want >= 0 && at >= want
}

// Compensate starts compensation from the current state. end is FAILED or
// EXPIRED.
func (s *Session) Compensate(reason string, detail map[string]any, end State, now time.Time) error {
	if !s.State.Compensable() {
		return fmt.Errorf("checkout: cannot compensate a session in %s", s.State)
	}
	s.FailedState, s.FailureReason, s.FailureDetail, s.EndState = s.State, reason, detail, end
	s.State = StateCompensating
	s.Attempts, s.NextAttemptAt, s.NextAction = 0, nil, nil
	s.UpdatedAt = now
	return nil
}

// Undone reports whether a compensation step has finished.
func (s Session) Undone(step string) bool {
	return slices.ContainsFunc(s.CompensationLog, func(e CompensationEntry) bool { return e.Step == step })
}

// Undo records a finished compensation step.
func (s *Session) Undo(step, result string, now time.Time) {
	if !s.Undone(step) {
		s.CompensationLog = append(s.CompensationLog, CompensationEntry{Step: step, Result: result, At: now})
	}
}

// RetryLater schedules the next attempt with exponential backoff, capped
// at limit.
func (s *Session) RetryLater(now time.Time, base, limit time.Duration) {
	s.Attempts++
	d := base
	for i := 1; i < s.Attempts && d < limit; i++ {
		d *= 2
	}
	d = min(d, limit)
	at := now.Add(d)
	s.NextAttemptAt = &at
}

// Violation is a confirm field that failed ADR-010 validation.
type Violation struct {
	Field   string
	Code    string
	Message string
}

// ConfirmInput is what the customer supplies at confirm. PaymentGateway
// switches to another of the shop's gateways; the service routes it
// before validating.
type ConfirmInput struct {
	GuestContact     *GuestContact
	DeliveryAddress  map[string]any
	BillingAddress   *BillingAddress
	ShippingOptionID string
	PaymentGateway   string
	PaymentToken     string
	ReturnURL        string
}

var e164RE = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)

// Limits.
const (
	MaxTokenLength     = 255
	MaxReturnURLLength = 2048
	maxAddressBytes    = 4096
)

// ValidateAddress checks a delivery address's format only (ADR-015): a
// non-empty JSON object of reasonable size. Its fields are the shop's.
func ValidateAddress(address map[string]any) []Violation {
	if len(address) == 0 {
		return []Violation{{"delivery_address", "required", "A delivery address is required for physical items"}}
	}
	if b, err := json.Marshal(address); err != nil || len(b) > maxAddressBytes {
		return []Violation{{"delivery_address", "invalid", "Delivery address is too large"}}
	}
	return nil
}

// ValidateConfirm checks the confirm input against the session (ADR-010):
// guests need an email and E.164 phone; physical items need shipping
// chosen for a delivery address first (the address given at confirm, if
// any, must be that one: the caller checks with AddressDiffers).
func (s Session) ValidateConfirm(in ConfirmInput) []Violation {
	var out []Violation
	add := func(field, code, msg string) { out = append(out, Violation{field, code, msg}) }
	if s.UserType == UserVisitor && in.GuestContact == nil {
		add("guest_contact", "required", "Guest checkout needs an email and a phone number")
	}
	if g := in.GuestContact; g != nil {
		if a, err := mail.ParseAddress(g.Email); err != nil || a.Address != g.Email || len(g.Email) > 254 {
			add("guest_contact.email", "invalid", "Email is not valid")
		}
		if !e164RE.MatchString(g.PhoneE164) {
			add("guest_contact.phone_e164", "invalid", "Phone must be in E.164 format, e.g. +27831234567")
		}
	}
	if s.RequiresShipping {
		if len(s.DeliveryAddress) == 0 {
			add("delivery_address", "required",
				"Physical items need a delivery address: choose shipping for it first (POST /checkout/{id}/shipping)")
		}
		id := in.ShippingOptionID
		if id == "" {
			id = s.ShippingOptionID
		}
		opt, ok := ShippingOption{}, false
		for _, o := range s.ShippingOptions {
			if o.ID == id {
				opt, ok = o, true
				break
			}
		}
		if !ok {
			add("shipping_option_id", "invalid", "Choose one of the session's shipping options")
		} else if opt.RequiresDeliverySlot && (s.DeliverySlot == nil || !s.DeliverySlot.Valid()) {
			add("delivery_slot", "required", "Choose a delivery slot (PUT /checkout/{id}/slot) for this shipping option")
		}
	}
	switch {
	case s.HostedPayment && in.PaymentToken != "":
		add("payment_gateway_token", "invalid", "The customer pays on the gateway's page; send no payment token")
	case !s.HostedPayment && (strings.TrimSpace(in.PaymentToken) == "" || len(in.PaymentToken) > MaxTokenLength):
		add("payment_gateway_token", "required", "A payment method token from the gateway is required")
	}
	switch {
	case len(in.ReturnURL) > MaxReturnURLLength:
		add("return_url", "invalid", "Return URL is too long")
	case s.HostedPayment && strings.TrimSpace(in.ReturnURL) == "":
		add("return_url", "required", "A return URL is required: the customer comes back to it after paying on the gateway's page")
	}
	if in.BillingAddress != nil {
		if err := ValidateBilling(*in.BillingAddress); err != nil {
			if e, ok := ycerrors.As(err); ok {
				for _, f := range e.Fields {
					add(f.Field, f.Code, f.Message)
				}
			} else {
				add("billing_address", "invalid", "Billing address is not valid")
			}
		}
	}
	return out
}

// Confirm applies a valid confirm input and starts the saga.
func (s *Session) Confirm(in ConfirmInput, key string, now time.Time) {
	s.GuestContact = in.GuestContact
	if in.BillingAddress != nil {
		b := NormaliseBilling(*in.BillingAddress)
		s.BillingAddress = &b
	}
	if s.RequiresShipping && in.ShippingOptionID != "" {
		s.SelectShipping(in.ShippingOptionID)
	}
	s.PaymentToken, s.ReturnURL = in.PaymentToken, in.ReturnURL
	s.ConfirmKey = key
	s.State = s.FirstStep()
	s.ConfirmedAt = &now
	s.UpdatedAt = now
}

// Finish ends compensation in the session's end state.
func (s *Session) Finish(now time.Time) {
	s.State = s.EndState
	if s.State == "" {
		s.State = StateFailed
	}
	s.PaymentToken, s.NextAction, s.NextAttemptAt = "", nil, nil
	s.UpdatedAt = now
}
