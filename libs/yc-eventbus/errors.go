package eventbus

import "errors"

// ErrPoison is returned by a consumer handler to signal that the message
// is unprocessable and should go straight to DLQ without retry.
//
// Wrap it with additional context when returning:
//
//	return fmt.Errorf("%w: unmarshal: %v", eventbus.ErrPoison, err)
var ErrPoison = errors.New("eventbus: poison message")

// IsPoison reports whether err wraps ErrPoison.
func IsPoison(err error) bool {
	return errors.Is(err, ErrPoison)
}
