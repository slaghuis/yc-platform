package domain

import "time"

// Tenant is a shop as analytics knows it.
type Tenant struct {
	TenantID      string
	Name          string
	StoreCurrency string
	ProvisionedAt time.Time
}

// Order is an order fact with its lines.
type Order struct {
	OrderID       string
	TenantID      string
	OrderNumber   string
	UserID        string
	UserType      string
	Currency      string
	ItemsMinor    int64
	TaxMinor      int64
	ShippingMinor int64
	TotalMinor    int64
	TransactionID string
	PlacedAt      time.Time
	Items         []OrderItem
}

// OrderItem is an order line.
type OrderItem struct {
	SKUID          string
	SKUCode        string
	Name           string
	Quantity       int64
	UnitPriceMinor int64
	LineTotalMinor int64
}

// PaymentOutcome is what a payment event says about a transaction.
type PaymentOutcome string

// Payment outcomes.
const (
	PaymentAuthorized PaymentOutcome = "authorized"
	PaymentSucceeded  PaymentOutcome = "succeeded"
	PaymentFailed     PaymentOutcome = "failed"
)

// Payment is a payment event on a transaction.
type Payment struct {
	TransactionID string
	TenantID      string
	Gateway       string
	Currency      string
	AmountMinor   int64
	Outcome       PaymentOutcome
	Reason        string
	At            time.Time
}

// Checkout states recorded as steps.
const (
	CheckoutStarted   = "started"
	CheckoutConfirmed = "confirmed"
	CheckoutCompleted = "completed"
	CheckoutFailed    = "failed"
	CheckoutExpired   = "expired"
)

// CartEvent is a cart event.
type CartEvent struct {
	CartID   string
	TenantID string
	Event    string
	CartType string
	InFunnel bool
	UserType string
	At       time.Time
}

// Cart events recorded.
const (
	CartCreated   = "created"
	CartAbandoned = "abandoned"
	CartConverted = "converted"
)
