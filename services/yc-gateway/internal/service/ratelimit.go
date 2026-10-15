package service

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/time/rate"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
)

// Quota is a token bucket: Requests per Per on average, up to Burst at
// once. Zero Requests means unlimited.
type Quota struct {
	Requests int           `koanf:"requests"`
	Per      time.Duration `koanf:"per"`
	Burst    int           `koanf:"burst"`
}

func (q Quota) enabled() bool { return q.Requests > 0 }

func (q Quota) limit() rate.Limit {
	return rate.Limit(float64(q.Requests) / q.Per.Seconds())
}

func (q Quota) validate(name string) error {
	if q.Requests < 0 || (q.enabled() && (q.Per <= 0 || q.Burst < 1)) {
		return fmt.Errorf("%s: requests must not be negative; a limited quota needs a positive per and burst", name)
	}
	return nil
}

// RateLimitConfig is the `rate_limit:` block (docs/services/yc-gateway.md,
// Rate Limiting).
type RateLimitConfig struct {
	Enabled bool `koanf:"enabled"`
	// Tenant limits all traffic to one tenant; TenantOverrides replaces it
	// for named tenants.
	Tenant          Quota            `koanf:"tenant"`
	TenantOverrides map[string]Quota `koanf:"tenant_overrides"`
	// Actor limits each signed-in user.
	Actor Quota `koanf:"actor"`
	// Visitor limits each visitor, and each client address that has no
	// credentials yet.
	Visitor Quota `koanf:"visitor"`
	// IdleTTL drops buckets unused for this long.
	IdleTTL time.Duration `koanf:"idle_ttl"`
}

// Validate reports an inconsistent quota.
func (c RateLimitConfig) Validate() error {
	errs := []error{
		c.Tenant.validate("rate_limit.tenant"), c.Actor.validate("rate_limit.actor"),
		c.Visitor.validate("rate_limit.visitor"),
	}
	for tenant, q := range c.TenantOverrides {
		errs = append(errs, q.validate("rate_limit.tenant_overrides."+tenant))
	}
	if c.IdleTTL <= 0 {
		errs = append(errs, errors.New("rate_limit.idle_ttl must be positive"))
	}
	return errors.Join(errs...)
}

// RateLimiter enforces per-tenant and per-actor quotas with in-memory token
// buckets. Each gateway instance counts on its own.
type RateLimiter struct {
	cfg RateLimitConfig
	now func() time.Time

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	lim  *rate.Limiter
	seen time.Time
}

// NewRateLimiter returns a limiter for cfg.
func NewRateLimiter(cfg RateLimitConfig, now func() time.Time) *RateLimiter {
	return &RateLimiter{cfg: cfg, now: now, buckets: map[string]*bucket{}, lastSweep: now()}
}

// Allow takes one request from the actor's bucket and the tenant's. When
// either is empty it takes nothing and returns how long to wait. actorKey
// identifies the actor within the tenant: a user ID, or a client address
// for callers without credentials (actorType visitor).
func (l *RateLimiter) Allow(tenantID string, actorType authctx.ActorType, actorKey string) (bool, time.Duration) {
	actorQuota := l.cfg.Actor
	if actorType == authctx.ActorVisitor {
		actorQuota = l.cfg.Visitor
	}
	tenantQuota, ok := l.cfg.TenantOverrides[tenantID]
	if !ok {
		tenantQuota = l.cfg.Tenant
	}

	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)

	var taken []*rate.Reservation
	for _, b := range []struct {
		key   string
		quota Quota
	}{
		{"actor:" + tenantID + ":" + actorKey, actorQuota},
		{"tenant:" + tenantID, tenantQuota},
	} {
		if !b.quota.enabled() {
			continue
		}
		r := l.bucket(b.key, b.quota, now).ReserveN(now, 1)
		if wait := r.DelayFrom(now); wait > 0 {
			r.CancelAt(now)
			for _, t := range taken {
				t.CancelAt(now)
			}
			return false, wait
		}
		taken = append(taken, r)
	}
	return true, 0
}

func (l *RateLimiter) bucket(key string, q Quota, now time.Time) *rate.Limiter {
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{lim: rate.NewLimiter(q.limit(), q.Burst)}
		l.buckets[key] = b
	}
	b.seen = now
	return b.lim
}

// sweep drops idle buckets at most once per IdleTTL. A dropped bucket
// comes back full, which an idle caller would have reached anyway once
// IdleTTL exceeds the time to refill.
func (l *RateLimiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < l.cfg.IdleTTL {
		return
	}
	for k, b := range l.buckets {
		if now.Sub(b.seen) >= l.cfg.IdleTTL {
			delete(l.buckets, k)
		}
	}
	l.lastSweep = now
}
