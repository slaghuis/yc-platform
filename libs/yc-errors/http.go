package errors

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"log/slog"
	"net/http"

	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
)

// ContentType is the media type of error responses (RFC 7807).
const ContentType = "application/problem+json"

// Problem is the problem+json body every error response carries. See
// docs/standards/api-conventions.md §6.
type Problem struct {
	Type          string       `json:"type"`
	Title         string       `json:"title"`
	Status        int          `json:"status"`
	Detail        string       `json:"detail,omitempty"`
	Code          Code         `json:"code"`
	CorrelationID string       `json:"correlation_id"`
	Errors        []FieldError `json:"errors,omitempty"`
}

// WriteError is the only place an error becomes an HTTP response. It
// classifies err, logs it once with the full chain, and writes problem+json
// with the user-safe parts only. Unclassified errors become 500 internal.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	ctx := r.Context()
	e := classify(err)

	logError(ctx, r, e, err)

	p := Problem{
		Type:          "yc:" + string(e.Code),
		Title:         e.Code.Title(),
		Status:        e.Status,
		Detail:        e.Message,
		Code:          e.Code,
		CorrelationID: telemetry.AuthAttrsFromContext(ctx).CorrelationID,
		Errors:        e.Fields,
	}
	w.Header().Set("Content-Type", ContentType)
	w.WriteHeader(e.Status)
	// The status line is already sent; an encode failure means the client
	// went away and there is nobody left to tell.
	_ = json.NewEncoder(w).Encode(p)
}

func classify(err error) *Error {
	if e, ok := As(err); ok {
		if e.Status == 0 {
			c := *e
			c.Status = e.Code.Status()
			return &c
		}
		return e
	}
	switch {
	case stderrors.Is(err, context.DeadlineExceeded):
		return UpstreamTimeout("deadline exceeded").Wrap(err)
	case stderrors.Is(err, context.Canceled):
		return Unavailable("Request cancelled").Wrap(err)
	default:
		return Internal("unclassified error").Wrap(err)
	}
}

func logError(ctx context.Context, r *http.Request, e *Error, original error) {
	attrs := []any{
		slog.String("code", string(e.Code)),
		slog.Int("http.status", e.Status),
		slog.String("http.method", r.Method),
		slog.Any("err", original),
	}
	if e.Internal != "" {
		attrs = append(attrs, slog.String("internal", e.Internal))
	}
	logger := telemetry.LoggerFromContext(ctx)
	switch {
	case stderrors.Is(original, context.Canceled):
		logger.Info("request cancelled", attrs...)
	case e.Status >= http.StatusInternalServerError:
		logger.Error("request failed", attrs...)
	default:
		logger.Info("request rejected", attrs...)
	}
}
