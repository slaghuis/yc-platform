package domain

import (
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
)

// MaxDeliveryAddresses is the maximum number of active book entries per customer (issue #32).
const MaxDeliveryAddresses = 5

const (
	maxAddressLabelRunes = 64
	maxAddressBytes      = 16 << 10
)

// DeliveryAddress is one customer address-book entry (issue #32).
type DeliveryAddress struct {
	ID           string
	UserID       string
	Label        string
	Address      map[string]any
	IsDefault    bool
	DeletedAt    *time.Time
	ReplacedByID string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Active reports whether the address is not soft-retired.
func (a DeliveryAddress) Active() bool { return a.DeletedAt == nil }

// DeliveryAddressInput is create / succession-edit body.
type DeliveryAddressInput struct {
	Label     string
	Address   map[string]any
	IsDefault bool
}

// NormaliseDeliveryAddressInput trims the label.
func NormaliseDeliveryAddressInput(in DeliveryAddressInput) DeliveryAddressInput {
	in.Label = strings.TrimSpace(in.Label)
	return in
}

// ValidateDeliveryAddressInput checks label and free-form address (ADR-015).
func ValidateDeliveryAddressInput(in DeliveryAddressInput) error {
	v := ycerrors.Validation()
	if utf8.RuneCountInString(in.Label) > maxAddressLabelRunes {
		v.Field("label", "invalid", "Label is too long")
	}
	if len(in.Address) == 0 {
		v.Field("address", "required", "A delivery address is required")
	} else if b, err := json.Marshal(in.Address); err != nil || len(b) > maxAddressBytes {
		v.Field("address", "invalid", "Delivery address is too large")
	}
	if err := v.Build(); len(err.Fields) > 0 {
		return err
	}
	return nil
}
