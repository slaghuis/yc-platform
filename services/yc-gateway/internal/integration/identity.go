// Package integration holds yc-gateway's clients for other services.
package integration

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	"github.com/slaghuis/yc-platform/services/yc-gateway/internal/domain"
)

// ErrInvalidToken means yc-identity rejected the token.
var ErrInvalidToken = errors.New("identity: invalid token")

// maxCacheEntries bounds the validation cache; when full, it is cleared.
const maxCacheEntries = 100_000

// Identity calls yc-identity and caches validation results in memory for
// up to the cache TTL (docs/services/yc-gateway.md): revocation takes
// effect within that window.
type Identity struct {
	baseURL string
	client  *http.Client
	ttl     time.Duration
	now     func() time.Time

	mu    sync.Mutex
	cache map[[sha256.Size]byte]cacheEntry
}

type cacheEntry struct {
	identity domain.Identity
	until    time.Time
}

// NewIdentity returns a client for yc-identity at baseURL.
func NewIdentity(baseURL string, timeout, cacheTTL time.Duration, now func() time.Time) *Identity {
	return &Identity{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		client: httpkit.NewClient(httpkit.ClientOptions{
			Timeout: timeout, PropagateAuth: true, CallerService: "yc-gateway",
		}),
		ttl:   cacheTTL,
		now:   now,
		cache: map[[sha256.Size]byte]cacheEntry{},
	}
}

type validateResponse struct {
	TenantID  string    `json:"tenant_id"`
	ActorID   string    `json:"actor_id"`
	ActorType string    `json:"actor_type"`
	TokenID   string    `json:"jti"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Validate returns the identity behind token, from cache when possible. It
// returns ErrInvalidToken when yc-identity rejects the token and an
// upstream error when yc-identity cannot answer.
func (c *Identity) Validate(ctx context.Context, token string) (domain.Identity, error) {
	key := sha256.Sum256([]byte(token))
	if id, ok := c.cached(key); ok {
		return id, nil
	}

	var resp validateResponse
	err := httpkit.DoJSON(ctx, c.client, http.MethodPost, c.baseURL+"/internal/validate-token",
		map[string]string{"token": token}, &resp)
	if httpkit.UpstreamStatus(err) == http.StatusUnauthorized {
		return domain.Identity{}, ErrInvalidToken
	}
	if err != nil {
		return domain.Identity{}, err
	}
	id := domain.Identity{
		Token:     token,
		TokenID:   resp.TokenID,
		TenantID:  resp.TenantID,
		ActorID:   resp.ActorID,
		ActorType: authctx.ActorType(resp.ActorType),
		ExpiresAt: resp.ExpiresAt,
	}
	c.store(key, id)
	return id, nil
}

type sessionResponse struct {
	AccessToken string    `json:"access_token"`
	ExpiresAt   time.Time `json:"expires_at"`
	UserID      string    `json:"user_id"`
	TenantID    string    `json:"tenant_id"`
}

// CreateVisitor asks yc-identity for a new visitor in tenantID.
func (c *Identity) CreateVisitor(ctx context.Context, tenantID string) (domain.Identity, error) {
	a, _ := authctx.FromContext(ctx)
	ctx = authctx.WithAuth(ctx, authctx.AuthContext{TenantID: tenantID, CorrelationID: a.CorrelationID})

	var resp sessionResponse
	if err := httpkit.DoJSON(ctx, c.client, http.MethodPost, c.baseURL+"/visitor", nil, &resp); err != nil {
		return domain.Identity{}, err
	}
	id := domain.Identity{
		Token:     resp.AccessToken,
		TenantID:  resp.TenantID,
		ActorID:   resp.UserID,
		ActorType: authctx.ActorVisitor,
		ExpiresAt: resp.ExpiresAt,
	}
	c.store(sha256.Sum256([]byte(id.Token)), id)
	return id, nil
}

func (c *Identity) cached(key [sha256.Size]byte) (domain.Identity, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.cache[key]
	if !ok {
		return domain.Identity{}, false
	}
	if !c.now().Before(e.until) {
		delete(c.cache, key)
		return domain.Identity{}, false
	}
	return e.identity, true
}

func (c *Identity) store(key [sha256.Size]byte, id domain.Identity) {
	until := c.now().Add(c.ttl)
	if !id.ExpiresAt.IsZero() && id.ExpiresAt.Before(until) {
		until = id.ExpiresAt
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cache) >= maxCacheEntries {
		clear(c.cache)
	}
	c.cache[key] = cacheEntry{identity: id, until: until}
}
