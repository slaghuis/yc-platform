package domain

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
)

// ReturnWindow is how long after delivery a customer may return items.
const ReturnWindow = 30 * 24 * time.Hour

// ReturnStatus is a return request's state.
type ReturnStatus string

// Return states.
const (
	ReturnRequested ReturnStatus = "requested"
	ReturnAccepted  ReturnStatus = "accepted"
	ReturnRejected  ReturnStatus = "rejected"
)

// ReturnItem is a quantity of one order line sent back.
type ReturnItem struct {
	SKUID    string `json:"sku_id"`
	Quantity int64  `json:"quantity"`
}

// Return is a customer's request to send items back and the shop's
// decision. No return shipment is booked and nothing is restocked.
type Return struct {
	ID             string
	OrderID        string
	Status         ReturnStatus
	Items          []ReturnItem
	Reason         string
	RefundShipping bool
	RefundMinor    int64
	DecisionReason string
	RequestedBy    string
	DecidedBy      string
	RequestedAt    time.Time
	DecidedAt      *time.Time
}

// Refund is one refund made on an order: an accepted return's, or an
// admin's goodwill refund.
type Refund struct {
	ID          string
	ReturnID    string
	AmountMinor int64
	Status      string
	Reason      string
	PaymentKey  string
	ActorID     string
	CreatedAt   time.Time
}

// CheckReturnable reports why the order cannot be returned now, if it
// cannot: it must be delivered, within ReturnWindow.
func (o Order) CheckReturnable(now time.Time) error {
	if o.Status != StatusDelivered || o.Shipment.DeliveredAt == nil {
		return ycerrors.New(ycerrors.CodeInvalidStateTransition,
			"Only a delivered order can be returned").With("order_status", string(o.Status))
	}
	if now.After(o.Shipment.DeliveredAt.Add(ReturnWindow)) {
		return ycerrors.New(ycerrors.CodeInvalidStateTransition,
			"The 30-day return window has closed").With("order_status", string(o.Status))
	}
	return nil
}

// line returns the order's line for a SKU.
func (o Order) line(skuID string) (Item, bool) {
	for _, it := range o.Items {
		if it.SKUID == skuID {
			return it, true
		}
	}
	return Item{}, false
}

// returned sums each SKU's quantity in the returns that count: accepted
// ones, and requested ones too when pending is set.
func returned(returns []Return, pending bool) map[string]int64 {
	out := map[string]int64{}
	for _, r := range returns {
		if r.Status == ReturnAccepted || (pending && r.Status == ReturnRequested) {
			for _, it := range r.Items {
				out[it.SKUID] += it.Quantity
			}
		}
	}
	return out
}

// ValidateReturn checks a return request against the order and its
// earlier returns: physical lines only, at most what is left of each.
func (o Order) ValidateReturn(items []ReturnItem, reason string, earlier []Return) error {
	v := ycerrors.Validation()
	if len(items) == 0 || len(items) > MaxItems {
		v.Field("items", "invalid", fmt.Sprintf("A return has 1 to %d lines", MaxItems))
	}
	already := returned(earlier, true)
	seen := map[string]bool{}
	for i, it := range items {
		f := fmt.Sprintf("items[%d]", i)
		line, ok := o.line(it.SKUID)
		switch {
		case uuid.Validate(it.SKUID) != nil || !ok:
			v.Field(f+".sku_id", "invalid", "The SKU is not on this order")
		case seen[it.SKUID]:
			v.Field(f+".sku_id", "duplicate", "Each SKU is listed once")
		case line.IsDigital:
			v.Field(f+".sku_id", "invalid", "Digital items are not returned")
		case it.Quantity <= 0:
			v.Field(f+".quantity", "invalid", "Quantity must be positive")
		case it.Quantity > line.Quantity-already[it.SKUID]:
			v.Field(f+".quantity", "invalid",
				fmt.Sprintf("At most %d can still be returned", max(line.Quantity-already[it.SKUID], 0)))
		}
		seen[it.SKUID] = true
	}
	if len(reason) > 500 {
		v.Field("reason", "too_long", "Reason is at most 500 characters")
	}
	if v.HasErrors() {
		return v.Build()
	}
	return nil
}

// ReturnRefund is what accepting r refunds: each line's share of its
// total, plus shipping when refundShipping is set and no earlier return
// refunded it, capped at what is left to refund. Shares are taken
// cumulatively, so returning a line in parts refunds exactly its total.
func (o Order) ReturnRefund(r Return, refundShipping bool, earlier []Return) int64 {
	before := returned(earlier, false)
	var amount int64
	for _, it := range r.Items {
		line, ok := o.line(it.SKUID)
		if !ok || line.Quantity <= 0 {
			continue
		}
		p := before[it.SKUID]
		amount += line.LineTotalMinor*(p+it.Quantity)/line.Quantity - line.LineTotalMinor*p/line.Quantity
	}
	if refundShipping && !ShippingRefunded(earlier) {
		amount += o.ShippingMinor
	}
	return min(amount, o.RefundableMinor())
}

// ShippingRefunded reports whether an accepted return refunded shipping.
func ShippingRefunded(returns []Return) bool {
	for _, r := range returns {
		if r.Status == ReturnAccepted && r.RefundShipping {
			return true
		}
	}
	return false
}

// FullyReturned reports whether the accepted returns cover every physical
// line.
func (o Order) FullyReturned(returns []Return) bool {
	got := returned(returns, false)
	physical := false
	for _, it := range o.Items {
		if it.IsDigital {
			continue
		}
		physical = true
		if got[it.SKUID] < it.Quantity {
			return false
		}
	}
	return physical
}

// ValidateRefund checks an admin's refund of amount.
func (o Order) ValidateRefund(amount int64, reason string) error {
	v := ycerrors.Validation()
	if amount <= 0 {
		v.Field("amount_minor", "invalid", "Amount must be positive")
	} else if amount > o.RefundableMinor() {
		v.Field("amount_minor", "invalid", fmt.Sprintf("At most %d is left to refund", o.RefundableMinor()))
	}
	if strings.TrimSpace(reason) == "" || len(reason) > 500 {
		v.Field("reason", "required", "Reason is required, at most 500 characters")
	}
	if v.HasErrors() {
		return v.Build()
	}
	if !o.Status.Placed() || o.PaymentTransactionID == "" {
		return ycerrors.New(ycerrors.CodeInvalidStateTransition, "This order has no payment to refund").
			With("order_status", string(o.Status))
	}
	return nil
}
