package httpkit

import (
	"encoding/json"
	stderrors "errors"
	"io"
	"net/http"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
)

// MaxBodyBytes caps request bodies read by DecodeJSON.
const MaxBodyBytes = 1 << 20

// WriteJSON writes v as JSON with status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// The status line is already sent; an encode failure means the client
	// went away.
	_ = json.NewEncoder(w).Encode(v)
}

// DecodeJSON decodes the request body into dst, rejecting unknown fields,
// trailing data and bodies over MaxBodyBytes. Failures are validation_failed
// errors ready for ycerrors.WriteError.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		switch {
		case stderrors.As(err, &tooLarge):
			return ycerrors.Validation().Message("Request body too large").Wrap(err)
		case stderrors.Is(err, io.EOF):
			return ycerrors.Validation().Message("Request body is required").Wrap(err)
		default:
			return ycerrors.Validation().Message("Invalid JSON").Wrap(err)
		}
	}
	if dec.More() {
		return ycerrors.Validation().Message("Invalid JSON").Wrap(stderrors.New("trailing data after JSON value"))
	}
	return nil
}
