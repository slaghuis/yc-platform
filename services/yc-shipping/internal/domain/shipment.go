package domain

import (
	"crypto/rand"
	"fmt"
	"strings"
	"time"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
)

// Status is a shipment's state.
type Status string

// Shipment states. A shipment starts label_created: drivers create the
// label when the shipment is created.
const (
	StatusLabelCreated Status = "label_created"
	StatusInTransit    Status = "in_transit"
	StatusDelivered    Status = "delivered"
	StatusException    Status = "exception"
	StatusCancelled    Status = "cancelled"
)

// next are the states each state may move to. In transit and exception
// may repeat, each with its own message.
var next = map[Status][]Status{
	StatusLabelCreated: {StatusInTransit, StatusException, StatusCancelled},
	StatusInTransit:    {StatusInTransit, StatusDelivered, StatusException},
	StatusException:    {StatusException, StatusInTransit, StatusDelivered},
}

// CanMove reports whether a shipment in s may move to to.
func (s Status) CanMove(to Status) bool {
	for _, n := range next[s] {
		if n == to {
			return true
		}
	}
	return false
}

// Dispatched reports whether the parcel has left the shop.
func (s Status) Dispatched() bool {
	return s == StatusInTransit || s == StatusDelivered || s == StatusException
}

// Shipment is one order's parcel.
type Shipment struct {
	ID               string
	OrderID          string
	OrderNumber      string
	RateID           string
	Method           string
	CarrierDriver    string
	TrackingNumber   string
	LabelURL         string
	Status           Status
	DeliveryAddress  map[string]any
	CarrierName      string
	CarrierReference string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Event is one step in a shipment's tracking history.
type Event struct {
	Status     Status
	Message    string
	OccurredAt time.Time
	RecordedAt time.Time
}

// Update is a status change a shop admin reports for a manual shipment.
type Update struct {
	Status           Status
	Message          string
	CarrierName      string
	CarrierReference string
	OccurredAt       time.Time
}

// Validate checks an update. An exception needs a message saying what
// went wrong.
func (u Update) Validate() error {
	v := ycerrors.Validation()
	switch u.Status {
	case StatusInTransit, StatusDelivered:
	case StatusException:
		if strings.TrimSpace(u.Message) == "" {
			v.Field("message", "required", "Say what went wrong")
		}
	case StatusLabelCreated, StatusCancelled:
		v.Field("status", "invalid", "Status is in_transit, delivered or exception")
	default:
		v.Field("status", "invalid", "Status is in_transit, delivered or exception")
	}
	if len(u.Message) > 500 {
		v.Field("message", "too_long", "Message is at most 500 characters")
	}
	if len(u.CarrierName) > 100 {
		v.Field("carrier_name", "too_long", "Carrier name is at most 100 characters")
	}
	if len(u.CarrierReference) > 100 {
		v.Field("carrier_reference", "too_long", "Carrier reference is at most 100 characters")
	}
	if v.HasErrors() {
		return v.Build()
	}
	return nil
}

// Apply moves the shipment on, or reports why it cannot.
func (s *Shipment) Apply(u Update, now time.Time) error {
	if !s.Status.CanMove(u.Status) {
		return ycerrors.New(ycerrors.CodeInvalidStateTransition,
			fmt.Sprintf("A shipment that is %s cannot become %s", s.Status, u.Status)).
			With("shipment_status", string(s.Status))
	}
	s.Status, s.UpdatedAt = u.Status, now
	if u.CarrierName != "" {
		s.CarrierName = strings.TrimSpace(u.CarrierName)
	}
	if u.CarrierReference != "" {
		s.CarrierReference = strings.TrimSpace(u.CarrierReference)
	}
	return nil
}

// trackingAlphabet is Crockford's base32: no I, L, O or U to misread.
const trackingAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewTrackingNumber returns a random tracking number, YC and twelve
// characters (60 bits).
func NewTrackingNumber() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	var sb strings.Builder
	sb.WriteString("YC")
	for _, c := range b {
		sb.WriteByte(trackingAlphabet[c%32])
	}
	return sb.String()
}

// ValidTrackingNumber reports whether s could be a tracking number, before
// it is looked up.
func ValidTrackingNumber(s string) bool {
	if len(s) < 4 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'A' || c > 'Z') && c != '-' {
			return false
		}
	}
	return true
}
