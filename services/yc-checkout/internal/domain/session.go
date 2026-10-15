// Package domain holds the checkout session and its saga state machine
// (ADR-019, Variant A).
package domain

import (
	"encoding/json"
	"fmt"
	"net/mail"
	"regexp"
	"slices"
	"strings"
	"time"
)

// State is a session's saga state.
type State string

// States. PRICING holds the quote until the customer confirms.
const (
	StatePricing      State = "PRICING"
	StateReserving    State = "RESERVING"
	StateAuthorizing  State = "AUTHORIZING"
	StateOrderCreate  State = "ORDER_CREATE"
	StateCapturing    State = "CAPTURING"
	StateConfirming   State = "CONFIRMING"
	StateCompleted    State = "COMPLETED"
	StateCompensating State = "COMPENSATING"
	StateFailed       State = "FAILED"
	StateExpired      State = "EXPIRED"
)

// States lists every state, in saga order.
var States = []State{
	StatePricing, StateReserving, StateAuthorizing, StateOrderCreate, StateCapturing,
	StateConfirming, StateCompleted, StateCompensating, StateFailed, StateExpired,
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
	case StateReserving, StateAuthorizing, StateOrderCreate, StateCapturing, StateConfirming, StateCompensating:
		return true
	case StatePricing, StateCompleted, StateFailed, StateExpired:
	}
	return false
}

// Compensable reports whether the session is short of the point of no
// return (capture succeeded), so a failure or expiry is compensated.
func (s State) Compensable() bool {
	switch s {
	case StatePricing, StateReserving, StateAuthorizing, StateOrderCreate, StateCapturing:
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
	// UnitPriceMinor is gross per unit, rounded; LineTotalMinor is the
	// line's gross, including TaxMinor.
	UnitPriceMinor int64 `json:"unit_price_minor"`
	LineTotalMinor int64 `json:"line_total_minor"`
	TaxMinor       int64 `json:"tax_minor"`
}

// ShippingOption is a way to deliver the physical items.
type ShippingOption struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	AmountMinor int64  `json:"amount_minor"`
}

// GuestContact is a visitor's contact details (ADR-010).
type GuestContact struct {
	Email     string `json:"email"`
	PhoneE164 string `json:"phone_e164"`
}

// Compensation steps.
const (
	UndoPayment     = "payment_voided"
	UndoOrder       = "order_abandoned"
	UndoReservation = "reservation_released"
)

// CompensationEntry records one finished compensation step.
type CompensationEntry struct {
	Step   string    `json:"step"`
	Result string    `json:"result"`
	At     time.Time `json:"at"`
}

// Session is a checkout session.
type Session struct {
	ID               string
	UserID           string
	UserType         UserType
	CartType         string
	CartID           string
	State            State
	Items            []Item
	RequiresShipping bool
	HasTrackedItems  bool
	Currency         string
	TaxInclusive     bool
	ItemsTotalMinor  int64
	TaxTotalMinor    int64
	ShippingMinor    int64
	TotalMinor       int64
	PricingSnapshot  json.RawMessage
	ShippingOptions  []ShippingOption
	ShippingOptionID string
	GuestContact     *GuestContact
	DeliveryAddress  map[string]any

	ConfirmKey     string
	PaymentGateway string
	PaymentToken   string
	ReturnURL      string

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
// totals. Shipping is untaxed.
func (s *Session) SelectShipping(id string) bool {
	for _, o := range s.ShippingOptions {
		if o.ID == id {
			s.ShippingOptionID = id
			s.ShippingMinor = o.AmountMinor
			s.TotalMinor = s.ItemsTotalMinor + s.ShippingMinor
			return true
		}
	}
	return false
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

// TrackedItems are the lines inventory reserves.
func (s Session) TrackedItems() []Item {
	var out []Item
	for _, it := range s.Items {
		if it.InventoryTracked {
			out = append(out, it)
		}
	}
	return out
}

// Key is the Idempotency-Key of a saga call: {session_id}:{step}
// (ADR-019 rule 5).
func (s Session) Key(step string) string { return s.ID + ":" + step }

// UndoKey is the Idempotency-Key of a compensation call.
func (s Session) UndoKey(step string) string { return s.Key(step) + ":undo" }

// FirstStep is where a confirmed session starts: RESERVING is skipped when
// no SKU is tracked (ADR-012).
func (s Session) FirstStep() State {
	if s.HasTrackedItems {
		return StateReserving
	}
	return StateAuthorizing
}

// Next is the state after the current step succeeds (Variant A).
func (s State) Next() State {
	switch s {
	case StateReserving:
		return StateAuthorizing
	case StateAuthorizing:
		return StateOrderCreate
	case StateOrderCreate:
		return StateCapturing
	case StateCapturing:
		return StateConfirming
	case StateConfirming:
		return StateCompleted
	case StatePricing, StateCompleted, StateCompensating, StateFailed, StateExpired:
	}
	return s
}

// Reached reports whether the saga got as far as step before failing.
func (s Session) Reached(step State) bool {
	order := []State{StateReserving, StateAuthorizing, StateOrderCreate, StateCapturing}
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

// ConfirmInput is what the customer supplies at confirm.
type ConfirmInput struct {
	GuestContact     *GuestContact
	DeliveryAddress  map[string]any
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

// ValidateConfirm checks the confirm input against the session (ADR-010):
// guests need an email and E.164 phone; physical items need a delivery
// address and a shipping option.
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
		if len(in.DeliveryAddress) == 0 {
			add("delivery_address", "required", "A delivery address is required for physical items")
		} else if b, err := json.Marshal(in.DeliveryAddress); err != nil || len(b) > maxAddressBytes {
			add("delivery_address", "invalid", "Delivery address is too large")
		}
		id := in.ShippingOptionID
		if id == "" {
			id = s.ShippingOptionID
		}
		if !slices.ContainsFunc(s.ShippingOptions, func(o ShippingOption) bool { return o.ID == id }) {
			add("shipping_option_id", "invalid", "Choose one of the session's shipping options")
		}
	}
	if strings.TrimSpace(in.PaymentToken) == "" || len(in.PaymentToken) > MaxTokenLength {
		add("payment_gateway_token", "required", "A payment method token from the gateway is required")
	}
	if len(in.ReturnURL) > MaxReturnURLLength {
		add("return_url", "invalid", "Return URL is too long")
	}
	return out
}

// Confirm applies a valid confirm input and starts the saga.
func (s *Session) Confirm(in ConfirmInput, key string, now time.Time) {
	s.GuestContact = in.GuestContact
	if s.RequiresShipping {
		s.DeliveryAddress = in.DeliveryAddress
		if in.ShippingOptionID != "" {
			s.SelectShipping(in.ShippingOptionID)
		}
	}
	s.PaymentGateway, s.PaymentToken, s.ReturnURL = in.PaymentGateway, in.PaymentToken, in.ReturnURL
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
