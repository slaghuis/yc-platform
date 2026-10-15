package domain

import "time"

// RevenueRow is revenue in one currency, for a date (daily series) or a
// whole range (totals), for one shop or every shop.
type RevenueRow struct {
	TenantID      string
	Date          time.Time
	Currency      string
	Orders        int64
	Cancelled     int64
	ItemsMinor    int64
	TaxMinor      int64
	ShippingMinor int64
	GrossMinor    int64
	RefundedMinor int64
	NetMinor      int64
}

// AverageOrderMinor is gross revenue per order.
func (r RevenueRow) AverageOrderMinor() int64 {
	if r.Orders == 0 {
		return 0
	}
	return r.GrossMinor / r.Orders
}

// FunnelRow counts each funnel stage.
type FunnelRow struct {
	TenantID           string
	Date               time.Time
	Visitors           int64
	CartsCreated       int64
	CheckoutsStarted   int64
	CheckoutsConfirmed int64
	OrdersPlaced       int64
}

// Conversion is orders per visitor, or orders per started checkout when
// no visitors were recorded.
func (f FunnelRow) Conversion() float64 {
	switch {
	case f.Visitors > 0:
		return float64(f.OrdersPlaced) / float64(f.Visitors)
	case f.CheckoutsStarted > 0:
		return float64(f.OrdersPlaced) / float64(f.CheckoutsStarted)
	}
	return 0
}

// ProductRow is a product's (SKU's) sales over a range.
type ProductRow struct {
	SKUID        string
	Name         string
	Currency     string
	Units        int64
	RevenueMinor int64
}

// GatewayRow is a payment gateway's performance over a range.
type GatewayRow struct {
	Gateway     string
	Currency    string
	Attempts    int64
	Successes   int64
	Failures    int64
	AmountMinor int64
}

// SuccessRate is successes per attempt.
func (g GatewayRow) SuccessRate() float64 {
	if g.Attempts == 0 {
		return 0
	}
	return float64(g.Successes) / float64(g.Attempts)
}

// Mismatch is an aggregate row that does not match its facts.
type Mismatch struct {
	Aggregate string
	TenantID  string
	Date      time.Time
}
