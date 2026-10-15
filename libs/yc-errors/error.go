package errors

import (
	stderrors "errors"
	"fmt"
	"strings"
)

// Error is the classified error every yc-platform service returns across
// layers. Only Code, Message and Fields ever reach a client; Internal and
// Cause are for logs.
type Error struct {
	Code     Code
	Status   int
	Message  string       // user-safe
	Internal string       // operator-only, logged, never returned
	Fields   []FieldError // for validation errors
	Cause    error        // wrapped underlying error
	// Extensions are extra problem members a client acts on, e.g. the
	// shortages of insufficient_stock (RFC 9457 §3.2). User-safe only.
	Extensions map[string]any
}

// FieldError is one field-level validation failure.
type FieldError struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// New returns an Error for code with the code's default status. Prefer the
// named constructors; New is for semantic codes such as
// CodeInsufficientStock.
func New(code Code, message string) *Error {
	return &Error{Code: code, Status: code.Status(), Message: message}
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(string(e.Code))
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.Internal != "" {
		b.WriteString(" (")
		b.WriteString(e.Internal)
		b.WriteString(")")
	}
	if e.Cause != nil {
		b.WriteString(": ")
		b.WriteString(e.Cause.Error())
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Cause }

// Wrap returns a copy of e with cause attached.
func (e *Error) Wrap(cause error) *Error {
	c := *e
	c.Cause = cause
	return &c
}

// WithInternal returns a copy of e with operator-only detail attached.
func (e *Error) WithInternal(format string, args ...any) *Error {
	c := *e
	c.Internal = fmt.Sprintf(format, args...)
	return &c
}

// With returns a copy of e with the problem extension member key set. The
// standard member names are reserved.
func (e *Error) With(key string, value any) *Error {
	if reservedMembers[key] {
		panic("ycerrors: reserved problem member " + key)
	}
	c := *e
	c.Extensions = make(map[string]any, len(e.Extensions)+1)
	for k, v := range e.Extensions {
		c.Extensions[k] = v
	}
	c.Extensions[key] = value
	return &c
}

var reservedMembers = map[string]bool{
	"type": true, "title": true, "status": true, "detail": true, "code": true, "correlation_id": true, "errors": true,
}

// As extracts the first *Error in err's chain.
func As(err error) (*Error, bool) {
	var e *Error
	if stderrors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// HasCode reports whether err's chain contains an *Error with code.
func HasCode(err error, code Code) bool {
	e, ok := As(err)
	return ok && e.Code == code
}

// NotFound reports that resource with id does not exist within the actor's
// scope. The id goes to Internal, not to the client.
func NotFound(resource, id string) *Error {
	return &Error{
		Code:     CodeNotFound,
		Status:   CodeNotFound.Status(),
		Message:  capitalise(resource) + " not found",
		Internal: resource + " " + id,
	}
}

// Conflict reports that resource state prevents the operation.
func Conflict(message string) *Error { return New(CodeConflict, message) }

// Unauthorized reports missing or invalid credentials.
func Unauthorized(message string) *Error { return New(CodeUnauthorized, message) }

// Forbidden reports an authenticated caller without permission.
func Forbidden(message string) *Error { return New(CodeForbidden, message) }

// Gone reports a resource that existed and will not return.
func Gone(message string) *Error { return New(CodeGone, message) }

// PreconditionFailed reports a failed optimistic concurrency check.
func PreconditionFailed(message string) *Error { return New(CodePreconditionFailed, message) }

// RateLimited reports too many requests.
func RateLimited(message string) *Error { return New(CodeRateLimited, message) }

// UpstreamFailure reports a failed call to another service. internal is the
// operator detail (target, status); upstream messages never reach clients.
func UpstreamFailure(internal string) *Error {
	return &Error{
		Code:     CodeUpstreamFailure,
		Status:   CodeUpstreamFailure.Status(),
		Message:  "An upstream service failed",
		Internal: internal,
	}
}

// UpstreamTimeout reports a call to another service that timed out.
func UpstreamTimeout(internal string) *Error {
	return &Error{
		Code:     CodeUpstreamTimeout,
		Status:   CodeUpstreamTimeout.Status(),
		Message:  "An upstream service timed out",
		Internal: internal,
	}
}

// Unavailable reports a deliberately unavailable service or tenant.
func Unavailable(message string) *Error { return New(CodeUnavailable, message) }

// Internal reports an unexpected failure. internal names the operation for
// operators; clients see a generic message.
func Internal(internal string) *Error {
	return &Error{
		Code:     CodeInternal,
		Status:   CodeInternal.Status(),
		Message:  "Internal error",
		Internal: internal,
	}
}

func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
