package integration

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"

	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
)

// ErrNoContactEmail means the shop has no contact email on record.
var ErrNoContactEmail = errors.New("tenant has no contact email")

// Tenant reads shop details from yc-tenant. Contact emails are cached for
// ttl, so a burst of messages makes one call.
type Tenant struct {
	client Caller
	ttl    time.Duration
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]cachedEmail
}

type cachedEmail struct {
	email   string
	expires time.Time
}

// NewTenant returns a client that caches contact emails for ttl (zero
// disables the cache).
func NewTenant(client Caller, ttl time.Duration) *Tenant {
	return &Tenant{client: client, ttl: ttl, now: time.Now, cache: map[string]cachedEmail{}}
}

type shop struct {
	ContactEmail string `json:"contact_email"`
}

// ContactEmail returns the shop's contact email, where it is notified of
// support activity. The call acts in tenantID, which must be on ctx.
func (t *Tenant) ContactEmail(ctx context.Context, tenantID string) (string, error) {
	now := t.now()
	t.mu.Lock()
	c, ok := t.cache[tenantID]
	t.mu.Unlock()
	if ok && now.Before(c.expires) {
		return c.email, nil
	}
	var out shop
	err := t.client.Call(ctx, http.MethodGet, "/internal/tenants/"+url.PathEscape(tenantID),
		httpkit.CallOptions{Attempts: 2}, nil, &out)
	if err != nil {
		return "", err
	}
	if out.ContactEmail == "" {
		return "", ErrNoContactEmail
	}
	if t.ttl > 0 {
		t.mu.Lock()
		t.cache[tenantID] = cachedEmail{email: out.ContactEmail, expires: now.Add(t.ttl)}
		t.mu.Unlock()
	}
	return out.ContactEmail, nil
}
