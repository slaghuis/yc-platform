package httpkit

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
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
	timeout  time.Duration
	client   *http.Client
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error

	mu      sync.Mutex
	token   string
	expires time.Time
}

// NewInternalClient returns a client for service clientID.
func NewInternalClient(clientID string, cfg InternalConfig) *InternalClient {
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	client := NewClient(ClientOptions{})
	// Bounded per attempt in Call instead, so one client serves calls with
	// different timeouts.
	client.Timeout = 0
	return &InternalClient{
		gateway:  strings.TrimSuffix(cfg.GatewayURL, "/"),
		clientID: clientID,
		secret:   cfg.ClientSecret,
		timeout:  cfg.Timeout,
		client:   client,
		now:      time.Now,
		sleep:    sleepCtx,
	}
}

// CallOptions configures InternalClient.Call.
type CallOptions struct {
	// IdempotencyKey is sent as Idempotency-Key on every attempt, so a
	// retry of a call whose outcome is unknown returns the stored result
	// (ADR-019 rule 5).
	IdempotencyKey string
	// Timeout bounds each attempt. Default the client's configured timeout.
	Timeout time.Duration
	// Attempts is the total number of tries for network errors, timeouts
	// and 5xx responses. Default 1. 4xx responses are never retried.
	Attempts int
	// Backoff is the delay before the first retry, doubled after each.
	// Default 200 ms.
	Backoff time.Duration
}

// Do sends in (if non-nil) as JSON to path on the gateway's internal
// listener and decodes a 2xx response into out (if non-nil), like DoJSON.
// The tenant, actor and correlation ID come from ctx (ADR-021 rule 4); the
// caller's own access token is never forwarded.
func (c *InternalClient) Do(ctx context.Context, method, path string, in, out any) error {
	return c.Call(ctx, method, path, CallOptions{}, in, out)
}

// Call is Do with an idempotency key, a timeout and retries.
func (c *InternalClient) Call(ctx context.Context, method, path string, opts CallOptions, in, out any) error {
	a, _ := authctx.FromContext(ctx)
	if a.TenantID == "" {
		return fmt.Errorf("httpkit: internal call %s %s: no tenant on context", method, path)
	}
	a.Token = ""
	if opts.Timeout == 0 {
		opts.Timeout = c.timeout
	}
	if opts.Attempts < 1 {
		opts.Attempts = 1
	}
	if opts.Backoff == 0 {
		opts.Backoff = 200 * time.Millisecond
	}
	var err error
	for attempt, reauthed := 0, false; attempt < opts.Attempts; attempt++ {
		if attempt > 0 {
			if serr := c.sleep(ctx, opts.Backoff<<(attempt-1)); serr != nil {
				return err
			}
		}
		var tok string
		tok, err = c.serviceToken(ctx)
		if err != nil {
			if retriable(ctx, err) {
				continue
			}
			return err
		}
		h := http.Header{}
		authctx.SetHeaders(h, a)
		h.Set(authctx.HeaderAuthorization, "Bearer "+tok)
		if opts.IdempotencyKey != "" {
			h.Set(HeaderIdempotencyKey, opts.IdempotencyKey)
		}
		err = c.attempt(ctx, opts.Timeout, method, path, h, in, out)
		if UpstreamStatus(err) == http.StatusUnauthorized && !reauthed {
			c.forget(tok)
			reauthed = true
			attempt--
			continue
		}
		if err == nil || !retriable(ctx, err) {
			return err
		}
	}
	return err
}

func (c *InternalClient) attempt(ctx context.Context, timeout time.Duration, method, path string, h http.Header, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return doJSON(ctx, c.client, method, c.gateway+path, h, in, out)
}

// retriable: a network error, timeout or 5xx, while the caller still waits.
func retriable(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	if ycerrors.HasCode(err, ycerrors.CodeUpstreamTimeout) {
		return true
	}
	status := UpstreamStatus(err)
	return ycerrors.HasCode(err, ycerrors.CodeUpstreamFailure) && (status == 0 || status >= http.StatusInternalServerError)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
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
