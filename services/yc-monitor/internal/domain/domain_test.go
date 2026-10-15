package domain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/slaghuis/yc-platform/services/yc-monitor/internal/domain"
)

func TestHealthTransitions(t *testing.T) {
	p := domain.HealthPolicy{FailuresToUnhealthy: 3, SuccessesToRecover: 3}
	now := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	s := domain.Service{Name: "yc-pricing", Status: domain.HealthUnknown}

	assert.Equal(t, domain.BecameHealthy, s.Observe("", now, p))
	assert.Equal(t, domain.NoTransition, s.Observe("", now, p))

	assert.Equal(t, domain.NoTransition, s.Observe("timeout", now, p))
	assert.Equal(t, domain.NoTransition, s.Observe("timeout", now, p))
	assert.Equal(t, domain.BecameUnhealthy, s.Observe("timeout", now, p))
	assert.Equal(t, domain.HealthUnhealthy, s.Status)
	assert.Equal(t, now, *s.UnhealthySince)
	assert.Equal(t, domain.NoTransition, s.Observe("timeout", now, p), "announced once")

	assert.Equal(t, domain.NoTransition, s.Observe("", now, p))
	assert.Equal(t, domain.NoTransition, s.Observe("timeout", now, p), "a failure resets the recovery count")
	assert.Equal(t, domain.NoTransition, s.Observe("", now, p))
	assert.Equal(t, domain.NoTransition, s.Observe("", now, p))
	assert.Equal(t, domain.Recovered, s.Observe("", now, p))
	assert.Equal(t, domain.HealthHealthy, s.Status)
	assert.Nil(t, s.UnhealthySince)
	assert.Empty(t, s.LastError)
}

func TestBuckets(t *testing.T) {
	at := time.Date(2026, 10, 15, 10, 37, 42, 0, time.FixedZone("SAST", 2*3600))
	assert.Equal(t, time.Date(2026, 10, 15, 8, 37, 0, 0, time.UTC), domain.Bucket1m.Start(at))
	assert.Equal(t, time.Date(2026, 10, 15, 8, 35, 0, 0, time.UTC), domain.Bucket5m.Start(at))
	assert.Equal(t, time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC), domain.Bucket1h.Start(at))
	assert.Equal(t, time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC), domain.Bucket1d.Start(at))
	_, err := domain.ParseBucket("2m")
	assert.Error(t, err)
}

func TestRules(t *testing.T) {
	assert.True(t, domain.Rules[domain.RuleDLQRate].Breached(11, 10))
	assert.False(t, domain.Rules[domain.RuleDLQRate].Breached(10, 10))
	assert.True(t, domain.Rules[domain.RuleCheckoutSuccessRate].Breached(0.4, 0.5))
	assert.False(t, domain.Rules[domain.RuleCheckoutSuccessRate].Breached(0.5, 0.5))
	for name, r := range domain.Rules {
		assert.Equal(t, name, r.Name)
		assert.NotEmpty(t, r.Notify, name)
	}
	assert.Equal(t, "gateway=stripe,tenant_id=shop-a", domain.Scope(map[string]string{"tenant_id": "shop-a", "gateway": "stripe"}))
	assert.Empty(t, domain.Scope(nil))
}

func TestAlertStatus(t *testing.T) {
	now := time.Now()
	a := domain.Alert{}
	assert.Equal(t, domain.AlertOpen, a.Status())
	a.AcknowledgedAt = &now
	assert.Equal(t, domain.AlertAcknowledged, a.Status())
	a.ResolvedAt = &now
	assert.Equal(t, domain.AlertResolved, a.Status())
}
