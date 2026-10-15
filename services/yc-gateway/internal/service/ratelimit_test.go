package service_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	"github.com/slaghuis/yc-platform/services/yc-gateway/internal/service"
)

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func limiter(c *clock) *service.RateLimiter {
	return service.NewRateLimiter(service.RateLimitConfig{
		Tenant:          service.Quota{Requests: 5, Per: time.Second, Burst: 5},
		TenantOverrides: map[string]service.Quota{"big": {Requests: 100, Per: time.Second, Burst: 100}},
		Actor:           service.Quota{Requests: 60, Per: time.Minute, Burst: 3},
		Visitor:         service.Quota{Requests: 30, Per: time.Minute, Burst: 2},
		IdleTTL:         time.Minute,
	}, c.Now)
}

func TestActorQuota(t *testing.T) {
	c := &clock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	l := limiter(c)

	for range 3 {
		ok, _ := l.Allow("big", authctx.ActorCustomer, "u1")
		require.True(t, ok)
	}
	ok, wait := l.Allow("big", authctx.ActorCustomer, "u1")
	assert.False(t, ok)
	assert.Equal(t, time.Second, wait, "60 per minute refills one a second")

	ok, _ = l.Allow("big", authctx.ActorCustomer, "u2")
	assert.True(t, ok, "other actors have their own bucket")
	ok, _ = l.Allow("other", authctx.ActorCustomer, "u1")
	assert.True(t, ok, "the same ID in another tenant is another actor")

	c.now = c.now.Add(time.Second)
	ok, _ = l.Allow("big", authctx.ActorCustomer, "u1")
	assert.True(t, ok)
}

func TestVisitorsAreStricter(t *testing.T) {
	c := &clock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	l := limiter(c)

	for range 2 {
		ok, _ := l.Allow("big", authctx.ActorVisitor, "v1")
		require.True(t, ok)
	}
	ok, wait := l.Allow("big", authctx.ActorVisitor, "v1")
	assert.False(t, ok)
	assert.Equal(t, 2*time.Second, wait)
}

func TestTenantQuota(t *testing.T) {
	c := &clock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	l := limiter(c)

	for i := range 5 {
		ok, _ := l.Allow("shop-a", authctx.ActorCustomer, string(rune('a'+i)))
		require.True(t, ok)
	}
	ok, wait := l.Allow("shop-a", authctx.ActorCustomer, "z")
	assert.False(t, ok)
	assert.Equal(t, 200*time.Millisecond, wait)

	c.now = c.now.Add(200 * time.Millisecond)
	ok, _ = l.Allow("shop-a", authctx.ActorCustomer, "z")
	assert.True(t, ok, "the refused request took nothing from z's bucket")
	for range 2 {
		ok, _ = l.Allow("shop-a", authctx.ActorCustomer, "z")
		if !ok {
			break
		}
	}
	assert.False(t, ok, "tenant bucket is empty again")

	ok, _ = l.Allow("shop-b", authctx.ActorCustomer, "a")
	assert.True(t, ok, "tenants are independent")
}

func TestDisabledQuota(t *testing.T) {
	l := service.NewRateLimiter(service.RateLimitConfig{IdleTTL: time.Minute}, time.Now)
	for range 1000 {
		ok, _ := l.Allow("shop-a", authctx.ActorVisitor, "v1")
		require.True(t, ok)
	}
}

func TestRateLimitConfigValidate(t *testing.T) {
	ok := service.RateLimitConfig{Tenant: service.Quota{Requests: 1, Per: time.Second, Burst: 1}, IdleTTL: time.Minute}
	require.NoError(t, ok.Validate())

	noBurst := ok
	noBurst.Actor = service.Quota{Requests: 1, Per: time.Second}
	require.Error(t, noBurst.Validate())

	badOverride := ok
	badOverride.TenantOverrides = map[string]service.Quota{"shop-a": {Requests: 1}}
	require.ErrorContains(t, badOverride.Validate(), "shop-a")

	noTTL := ok
	noTTL.IdleTTL = 0
	require.Error(t, noTTL.Validate())
}
