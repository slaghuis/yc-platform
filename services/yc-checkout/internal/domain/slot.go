package domain

import (
	"strings"
	"time"
)

// MaxSlotListDays caps GET /checkout/{id}/slots ranges (issue #30).
const MaxSlotListDays = 14

// DeliverySlot is the customer's chosen delivery window (issue #30).
type DeliverySlot struct {
	SlotID     string    `json:"slot_id"`
	StartAt    time.Time `json:"start_at"`
	EndAt      time.Time `json:"end_at"`
	Timezone   string    `json:"timezone"`
	Label      string    `json:"label,omitempty"`
	ScheduleID string    `json:"schedule_id,omitempty"`
}

// AvailableSlot is one window returned by list availability.
type AvailableSlot struct {
	SlotID            string    `json:"slot_id"`
	StartAt           time.Time `json:"start_at"`
	EndAt             time.Time `json:"end_at"`
	Timezone          string    `json:"timezone"`
	RemainingCapacity int       `json:"remaining_capacity"`
	Label             string    `json:"label,omitempty"`
}

// NormaliseDeliverySlot trims fields; empty timezone defaults to Africa/Johannesburg.
func NormaliseDeliverySlot(s DeliverySlot) DeliverySlot {
	s.SlotID = strings.TrimSpace(s.SlotID)
	s.Timezone = strings.TrimSpace(s.Timezone)
	s.Label = strings.TrimSpace(s.Label)
	s.ScheduleID = strings.TrimSpace(s.ScheduleID)
	if s.Timezone == "" {
		s.Timezone = "Africa/Johannesburg"
	}
	s.StartAt = s.StartAt.UTC()
	s.EndAt = s.EndAt.UTC()
	return s
}

// Valid reports whether a bound slot has the required identity and times.
func (s DeliverySlot) Valid() bool {
	return s.SlotID != "" && !s.StartAt.IsZero() && !s.EndAt.IsZero() && s.EndAt.After(s.StartAt)
}

// NeedsDeliverySlot reports whether the selected shipping option requires a slot.
func (s Session) NeedsDeliverySlot() bool {
	o, ok := s.SelectedShipping()
	return ok && o.RequiresDeliverySlot
}
