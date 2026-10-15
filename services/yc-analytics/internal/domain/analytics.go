// Package domain holds yc-analytics' types and rules.
package domain

import (
	"fmt"
	"slices"
	"time"
)

// DateLayout is how dates are written: 2026-10-15.
const DateLayout = time.DateOnly

// Date is the UTC calendar date of t.
func Date(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// MaxRangeDays caps a query's date range.
const MaxRangeDays = 366

// Range is an inclusive range of UTC dates.
type Range struct {
	From, To time.Time
}

// DefaultRange is the 30 days ending today.
func DefaultRange(now time.Time) Range {
	to := Date(now)
	return Range{From: to.AddDate(0, 0, -29), To: to}
}

// NewRange fills unset ends from the default range and checks the result.
func NewRange(from, to *time.Time, now time.Time) (Range, error) {
	r := DefaultRange(now)
	if to != nil {
		r.To = Date(*to)
		if from == nil {
			r.From = r.To.AddDate(0, 0, -29)
		}
	}
	if from != nil {
		r.From = Date(*from)
	}
	if r.From.After(r.To) {
		return r, fmt.Errorf("from %s is after to %s", r.From.Format(DateLayout), r.To.Format(DateLayout))
	}
	if r.Days() > MaxRangeDays {
		return r, fmt.Errorf("the range is %d days; at most %d", r.Days(), MaxRangeDays)
	}
	return r, nil
}

// Days is the number of dates in the range.
func (r Range) Days() int { return int(r.To.Sub(r.From).Hours()/24) + 1 }

// Aggregate names.
const (
	AggDailyRevenue       = "agg_daily_revenue"
	AggDailyFunnel        = "agg_daily_funnel"
	AggProductPerformance = "agg_product_performance"
	AggGatewayPerformance = "agg_gateway_performance"
)

// Aggregates lists every aggregate.
var Aggregates = []string{AggDailyRevenue, AggDailyFunnel, AggProductPerformance, AggGatewayPerformance}

// Day is a shop's date, the unit aggregates are rebuilt in.
type Day struct {
	TenantID string
	Date     time.Time
}

// Milestone kinds (analytics.milestone.reached.v1).
const (
	MilestoneFirstOrder = "first_order"
	MilestoneOrderCount = "order_count"
	MilestoneRevenue    = "revenue"
)

// MilestoneRules says which milestones are announced.
type MilestoneRules struct {
	FirstOrder  bool
	OrderCounts []int64
	// RevenueTiers are in minor units of the store currency.
	RevenueTiers []int64
	// MaxAge skips orders placed longer ago, so replaying old events
	// (e.g. when yc-analytics first starts) announces nothing.
	MaxAge time.Duration
}

// Milestone is a milestone reached.
type Milestone struct {
	Kind      string
	Threshold int64
	Currency  string
}

// Reached returns the milestones an order crossed: the shop had before
// orders (and beforeRevenue in the store currency) and now has after.
func (m MilestoneRules) Reached(before, after, beforeRevenue, afterRevenue int64, currency string) []Milestone {
	var out []Milestone
	if m.FirstOrder && before == 0 && after >= 1 {
		out = append(out, Milestone{Kind: MilestoneFirstOrder, Threshold: 1})
	}
	for _, n := range m.OrderCounts {
		if before < n && after >= n {
			out = append(out, Milestone{Kind: MilestoneOrderCount, Threshold: n})
		}
	}
	if currency == "" {
		return out
	}
	for _, n := range m.RevenueTiers {
		if beforeRevenue < n && afterRevenue >= n {
			out = append(out, Milestone{Kind: MilestoneRevenue, Threshold: n, Currency: currency})
		}
	}
	return out
}

// Export queries (GET /admin/analytics/export/{query}).
const (
	QueryRevenue     = "revenue"
	QueryTopProducts = "top_products"
	QueryFunnel      = "funnel"
	QueryGateways    = "gateways"
)

// ValidQuery reports whether q can be exported.
func ValidQuery(q string) bool {
	return slices.Contains([]string{QueryRevenue, QueryTopProducts, QueryFunnel, QueryGateways}, q)
}
