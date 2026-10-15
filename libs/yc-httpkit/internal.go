package httpkit

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
)

// InternalConfig is a service's `internal_calls:` block (ADR-021). The
// client ID is the service name.
type InternalConfig struct {
	// GatewayURL is yc-gateway's internal listener.
	GatewayURL   string        `koanf:"gateway_url"`
	ClientSecret string        `koanf:"client_secret"`
	Timeout      time.Duration `koanf:"timeout"`
}

// tokenRenewBefore is how long before expiry a cached service token is
// replaced.
const tokenRenewBefore = time.Minute

// InternalClient calls other services' /internal/ endpoints through
// yc-gateway's internal listener with a service token (ADR-021 rule 6).
type InternalClient struct {
	gateway  string
	clientID string
	secret   string
	client   *http.Client
	now      func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

// NewInternalClient returns a client for service clientID.
func NewInternalClient(clientID string, cfg InternalConfig) *InternalClient {
	return &InternalClient{
		gateway:  strings.TrimSuffix(cfg.GatewayURL, "/"),
		clientID: clientID,
		secret:   cfg.ClientSecret,
		client:   NewClient(ClientOptions{Timeout: cfg.Timeout}),
		now:      time.Now,
	}
}

// Do sends in (if non-nil) as JSON to path on the gateway's internal
// listener and decodes a 2xx response into out (if non-nil), like DoJSON.
// The tenant, actor and correlation ID come from ctx (ADR-021 rule 4); the
// caller's own access token is never forwarded.
func (c *InternalClient) Do(ctx context.Context, method, path string, in, out any) error {
	a, _ := authctx.FromContext(ctx)
	if a.TenantID == "" {
		return fmt.Errorf("httpkit: internal call %s %s: no tenant on context", method, path)
	}
	a.Token = ""
	for attempt := 0; ; attempt++ {
		tok, err := c.serviceToken(ctx)
		if err != nil {
			return err
		}
		h := http.Header{}
		authctx.SetHeaders(h, a)
		h.Set(authctx.HeaderAuthorization, "Bearer "+tok)
		err = doJSON(ctx, c.client, method, c.gateway+path, h, in, out)
		if attempt == 0 && UpstreamStatus(err) == http.StatusUnauthorized {
			c.forget(tok)
			continue
		}
		return err
	}
}

type serviceTokenResponse struct {
	AccessToken string    `json:"access_token"`
	ExpiresAt   time.Time `json:"expires_at"`
}

func (c *InternalClient) serviceToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.now().Before(c.expires.Add(-tokenRenewBefore)) {
		return c.token, nil
	}
	var resp serviceTokenResponse
	err := DoJSON(ctx, c.client, http.MethodPost, c.gateway+"/internal/service-token",
		map[string]string{"client_id": c.clientID, "client_secret": c.secret}, &resp)
	if err != nil {
		return "", fmt.Errorf("httpkit: service token for %s: %w", c.clientID, err)
	}
	c.token, c.expires = resp.AccessToken, resp.ExpiresAt
	return c.token, nil
}

func (c *InternalClient) forget(tok string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == tok {
		c.token = ""
	}
}
