package errors

import "net/http"

// Code is the stable, machine-readable error identifier returned to clients
// and used by operators. See docs/standards/error-handling.md §2.
type Code string

// Operational error codes.
const (
	CodeNotFound           Code = "not_found"
	CodeConflict           Code = "conflict"
	CodeValidationFailed   Code = "validation_failed"
	CodeUnauthorized       Code = "unauthorized"
	CodeForbidden          Code = "forbidden"
	CodeGone               Code = "gone"
	CodePreconditionFailed Code = "precondition_failed"
	CodeRateLimited        Code = "rate_limited"
	CodeUpstreamFailure    Code = "upstream_failure"
	CodeUpstreamTimeout    Code = "upstream_timeout"
	CodeUnavailable        Code = "unavailable"
	CodeInternal           Code = "internal"
)

// Semantic error codes. Their status is chosen per code; see codeInfo.
const (
	CodeInsufficientStock      Code = "insufficient_stock"
	CodePriceQuoteExpired      Code = "price_quote_expired"
	CodePaymentDeclined        Code = "payment_declined"
	CodeGatewayUnavailable     Code = "gateway_unavailable"
	CodeTenantSuspended        Code = "tenant_suspended"
	CodeInvalidStateTransition Code = "invalid_state_transition"
)

type codeMeta struct {
	status int
	title  string
}

var codeInfo = map[Code]codeMeta{
	CodeNotFound:           {http.StatusNotFound, "Not found"},
	CodeConflict:           {http.StatusConflict, "Conflict"},
	CodeValidationFailed:   {http.StatusBadRequest, "Validation failed"},
	CodeUnauthorized:       {http.StatusUnauthorized, "Unauthorized"},
	CodeForbidden:          {http.StatusForbidden, "Forbidden"},
	CodeGone:               {http.StatusGone, "Gone"},
	CodePreconditionFailed: {http.StatusPreconditionFailed, "Precondition failed"},
	CodeRateLimited:        {http.StatusTooManyRequests, "Too many requests"},
	CodeUpstreamFailure:    {http.StatusBadGateway, "Upstream failure"},
	CodeUpstreamTimeout:    {http.StatusGatewayTimeout, "Upstream timeout"},
	CodeUnavailable:        {http.StatusServiceUnavailable, "Service unavailable"},
	CodeInternal:           {http.StatusInternalServerError, "Internal error"},

	CodeInsufficientStock:      {http.StatusConflict, "Insufficient stock"},
	CodePriceQuoteExpired:      {http.StatusConflict, "Price quote expired"},
	CodePaymentDeclined:        {http.StatusBadRequest, "Payment declined"},
	CodeGatewayUnavailable:     {http.StatusServiceUnavailable, "Payment gateway unavailable"},
	CodeTenantSuspended:        {http.StatusServiceUnavailable, "Tenant suspended"},
	CodeInvalidStateTransition: {http.StatusConflict, "Invalid state transition"},
}

// Status returns the HTTP status for the code, or 500 for an unknown code.
func (c Code) Status() int {
	if m, ok := codeInfo[c]; ok {
		return m.status
	}
	return http.StatusInternalServerError
}

// Title returns the human-readable summary used as the problem+json title.
func (c Code) Title() string {
	if m, ok := codeInfo[c]; ok {
		return m.title
	}
	return "Error"
}

// Known reports whether c is a registered code.
func (c Code) Known() bool {
	_, ok := codeInfo[c]
	return ok
}
