package domain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/slaghuis/yc-platform/services/yc-analytics/internal/domain"
)

func date(s string) time.Time {
	t, err := time.Parse(domain.DateLayout, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestNewRange(t *testing.T) {
	now := time.Date(2026, 10, 15, 23, 30, 0, 0, time.FixedZone("SAST", 2*3600))

	r, err := domain.NewRange(nil, nil, now)
	require.NoError(t, err)
	assert.Equal(t, date("2026-09-16"), r.From, "30 days ending today (UTC)")
	assert.Equal(t, date("2026-10-15"), r.To)
	assert.Equal(t, 30, r.Days())

	to := date("2026-03-31")
	r, err = domain.NewRange(nil, &to, now)
	require.NoError(t, err)
	assert.Equal(t, date("2026-03-02"), r.From)

	from := date("2026-10-01")
	r, err = domain.NewRange(&from, nil, now)
	require.NoError(t, err)
	assert.Equal(t, 15, r.Days())

	from, to = date("2026-10-02"), date("2026-10-01")
	_, err = domain.NewRange(&from, &to, now)
	require.Error(t, err)

	from, to = date("2025-01-01"), date("2026-01-02")
	_, err = domain.NewRange(&from, &to, now)
	require.ErrorContains(t, err, "at most 366")
}

func TestMilestonesReached(t *testing.T) {
	rules := domain.MilestoneRules{
		FirstOrder: true, OrderCounts: []int64{10, 100}, RevenueTiers: []int64{100_000, 1_000_000},
	}

	assert.Equal(t, []domain.Milestone{{Kind: domain.MilestoneFirstOrder, Threshold: 1}},
		rules.Reached(0, 1, 0, 5_000, "ZAR"))
	assert.Empty(t, rules.Reached(1, 2, 5_000, 10_000, "ZAR"))
	assert.Equal(t, []domain.Milestone{
		{Kind: domain.MilestoneOrderCount, Threshold: 10},
		{Kind: domain.MilestoneRevenue, Threshold: 100_000, Currency: "ZAR"},
	}, rules.Reached(9, 10, 90_000, 150_000, "ZAR"))
	assert.Equal(t, []domain.Milestone{
		{Kind: domain.MilestoneRevenue, Threshold: 100_000, Currency: "ZAR"},
		{Kind: domain.MilestoneRevenue, Threshold: 1_000_000, Currency: "ZAR"},
	}, rules.Reached(20, 21, 50_000, 2_000_000, "ZAR"), "one large order crosses two tiers")
	assert.Equal(t, []domain.Milestone{{Kind: domain.MilestoneOrderCount, Threshold: 10}},
		rules.Reached(9, 10, 0, 500_000, ""), "no revenue milestones without a currency")
}

func TestRates(t *testing.T) {
	assert.InDelta(t, 0.25, domain.FunnelRow{Visitors: 8, OrdersPlaced: 2}.Conversion(), 1e-9)
	assert.InDelta(t, 0.5, domain.FunnelRow{CheckoutsStarted: 4, OrdersPlaced: 2}.Conversion(), 1e-9)
	assert.Zero(t, domain.FunnelRow{}.Conversion())
	assert.InDelta(t, 0.75, domain.GatewayRow{Attempts: 4, Successes: 3}.SuccessRate(), 1e-9)
	assert.Zero(t, domain.GatewayRow{}.SuccessRate())
	assert.Equal(t, int64(150), domain.RevenueRow{Orders: 2, GrossMinor: 300}.AverageOrderMinor())
	assert.Zero(t, domain.RevenueRow{}.AverageOrderMinor())
}

func TestValidQuery(t *testing.T) {
	for _, q := range []string{"revenue", "top_products", "funnel", "gateways", "cart_recovery"} {
		assert.True(t, domain.ValidQuery(q), q)
	}
	assert.False(t, domain.ValidQuery("cohorts"))
}

func TestCartRecoveryRates(t *testing.T) {
	assert.InDelta(t, 0.5, domain.AbandonmentRateOf(2, 4), 1e-9)
	assert.Zero(t, domain.AbandonmentRateOf(1, 0))
	assert.InDelta(t, 0.25, domain.RecoveryRateOf(1, 4), 1e-9)
	assert.Zero(t, domain.RecoveryRateOf(1, 0))
}
