package httpkit

import (
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
)

// ClientOptions configures NewClient.
type ClientOptions struct {
	// Timeout bounds each request. Default 10 s.
	Timeout time.Duration

	// PropagateAuth forwards the caller's tenant, actor and access token
	// from the context. Only for calls to yc-platform services; never set
	// it on clients for third parties (OAuth, payment gateways).
	PropagateAuth bool

	// CallerService sets X-YC-Caller-Service on every request. Only
	// yc-gateway calls services directly and sets it; everything else goes
	// through InternalClient (ADR-021).
	CallerService string
}

// NewClient returns an HTTP client that creates a span per call, injects
// W3C trace context and X-YC-Correlation-ID, and optionally forwards auth.
func NewClient(opts ClientOptions) *http.Client {
	if opts.Timeout == 0 {
		opts.Timeout = 10 * time.Second
	}
	return &http.Client{
		Timeout: opts.Timeout,
		Transport: otelhttp.NewTransport(&propagatingTransport{
			next:          http.DefaultTransport,
			propagateAuth: opts.PropagateAuth,
			callerService: opts.CallerService,
		}),
	}
}

type propagatingTransport struct {
	next          http.RoundTripper
	propagateAuth bool
	callerService string
}

func (t *propagatingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	a, ok := authctx.FromContext(req.Context())
	if !ok && t.callerService == "" {
		return t.next.RoundTrip(req)
	}
	req = req.Clone(req.Context())
	if t.callerService != "" {
		req.Header.Set(authctx.HeaderCallerService, t.callerService)
	}
	if !ok {
		return t.next.RoundTrip(req)
	}
	if a.CorrelationID != "" && req.Header.Get(authctx.HeaderCorrelationID) == "" {
		req.Header.Set(authctx.HeaderCorrelationID, a.CorrelationID)
	}
	if t.propagateAuth {
		authctx.SetHeaders(req.Header, a)
	}
	return t.next.RoundTrip(req)
}

// StatusError is a non-2xx response from DoJSON. It is wrapped in an
// upstream_failure error; use errors.As to inspect the status.
type StatusError struct {
	StatusCode int
	Body       []byte
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("upstream status %d", e.StatusCode)
}

// UpstreamStatus returns the status of a non-2xx DoJSON response in err's
// chain, or 0.
func UpstreamStatus(err error) int {
	var se *StatusError
	if stderrors.As(err, &se) {
		return se.StatusCode
	}
	return 0
}

// UpstreamProblem decodes the problem+json body of a non-2xx response in
// err's chain, e.g. to act on an upstream insufficient_stock. False when
// there is no response or its body is not a problem.
func UpstreamProblem(err error) (ycerrors.Problem, bool) {
	var se *StatusError
	if !stderrors.As(err, &se) {
		return ycerrors.Problem{}, false
	}
	var p ycerrors.Problem
	if json.Unmarshal(se.Body, &p) != nil || p.Code == "" {
		return ycerrors.Problem{}, false
	}
	return p, true
}

const maxErrorBody = 16 << 10

// DoJSON sends in (if non-nil) as JSON and decodes a 2xx response into out
// (if non-nil). Failures are classified for ycerrors.WriteError: timeouts
// as upstream_timeout, everything else, including non-2xx responses, as
// upstream_failure wrapping a *StatusError where applicable.
func DoJSON(ctx context.Context, c *http.Client, method, url string, in, out any) error {
	return doJSON(ctx, c, method, url, nil, in, out)
}

func doJSON(ctx context.Context, c *http.Client, method, url string, header http.Header, in, out any) error {
	var body io.Reader = http.NoBody
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("httpkit: marshal request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return fmt.Errorf("httpkit: new request: %w", err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.Do(req)
	if err != nil {
		if stderrors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return ycerrors.UpstreamTimeout(method + " " + url).Wrap(err)
		}
		return ycerrors.UpstreamFailure(method + " " + url).Wrap(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return ycerrors.UpstreamFailure(fmt.Sprintf("%s %s: status %d", method, url, resp.StatusCode)).
			Wrap(&StatusError{StatusCode: resp.StatusCode, Body: b})
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return ycerrors.UpstreamFailure(method + " " + url + ": decode response").Wrap(err)
	}
	return nil
}

func isTimeout(err error) bool {
	var t interface{ Timeout() bool }
	return stderrors.As(err, &t) && t.Timeout()
}
