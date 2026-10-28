package domain

import (
	"strings"
	"time"
)

// DeliverySlot is the snapshotted delivery window on an order (issue #30).
type DeliverySlot struct {
	SlotID     string    `json:"slot_id"`
	StartAt    time.Time `json:"start_at"`
	EndAt      time.Time `json:"end_at"`
	Timezone   string    `json:"timezone"`
	Label      string    `json:"label,omitempty"`
	ScheduleID string    `json:"schedule_id,omitempty"`
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
