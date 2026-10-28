package domain

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
)

// DefaultScheduleTimezone is used when a schedule omits timezone (SA MVP).
const DefaultScheduleTimezone = "Africa/Johannesburg"

// Bounds for schedule windows and list ranges.
const (
	MinWindowMinutes = 5
	MaxWindowMinutes = 240
	MaxSlotListDays  = 14
)

// Schedule is a shop's rules for generating delivery time windows.
// Weekdays use Go's time.Weekday: 0 = Sunday … 6 = Saturday.
type Schedule struct {
	ID                string
	Name              string
	Timezone          string
	WindowMinutes     int
	Weekdays          []int
	OpenTime          string // HH:MM local
	CloseTime         string // HH:MM local
	CapacityPerWindow int
	MinLeadMinutes    int
	SameDayCutoff     *string  // HH:MM local; nil = no cut-off
	BlackoutDates     []string // YYYY-MM-DD local
	Active            bool
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Slot is one generated delivery window with remaining capacity.
type Slot struct {
	SlotID            string
	ScheduleID        string
	StartAt           time.Time
	EndAt             time.Time
	Timezone          string
	RemainingCapacity int
}

// Normalise trims fields and applies the default timezone when empty.
func (s *Schedule) Normalise() {
	s.Name = strings.TrimSpace(s.Name)
	s.Timezone = strings.TrimSpace(s.Timezone)
	if s.Timezone == "" {
		s.Timezone = DefaultScheduleTimezone
	}
	s.OpenTime = strings.TrimSpace(s.OpenTime)
	s.CloseTime = strings.TrimSpace(s.CloseTime)
	if s.SameDayCutoff != nil {
		c := strings.TrimSpace(*s.SameDayCutoff)
		if c == "" {
			s.SameDayCutoff = nil
		} else {
			s.SameDayCutoff = &c
		}
	}
	out := make([]string, 0, len(s.BlackoutDates))
	seen := map[string]bool{}
	for _, d := range s.BlackoutDates {
		d = strings.TrimSpace(d)
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	s.BlackoutDates = out
}

// Validate checks a schedule from a shop admin.
func (s Schedule) Validate() error {
	v := ycerrors.Validation()
	if s.Name == "" || len(s.Name) > 100 {
		v.Field("name", "invalid", "Name is 1 to 100 characters")
	}
	tz := s.Timezone
	if tz == "" {
		tz = DefaultScheduleTimezone
	}
	if _, err := time.LoadLocation(tz); err != nil {
		v.Field("timezone", "invalid", "Timezone must be a loadable IANA name")
	}
	if s.WindowMinutes < MinWindowMinutes || s.WindowMinutes > MaxWindowMinutes {
		v.Field("window_minutes", "invalid", fmt.Sprintf("Window is %d to %d minutes", MinWindowMinutes, MaxWindowMinutes))
	}
	if len(s.Weekdays) == 0 {
		v.Field("weekdays", "invalid", "At least one weekday is required")
	}
	seenDay := map[int]bool{}
	for i, d := range s.Weekdays {
		if d < 0 || d > 6 {
			v.Field(fmt.Sprintf("weekdays[%d]", i), "invalid", "Weekday is 0 (Sunday) to 6 (Saturday)")
		} else if seenDay[d] {
			v.Field(fmt.Sprintf("weekdays[%d]", i), "invalid", "Weekdays must be unique")
		}
		seenDay[d] = true
	}
	openM, openOK := parseClock(s.OpenTime)
	if !openOK {
		v.Field("open_time", "invalid", "Open time is HH:MM")
	}
	closeM, closeOK := parseClock(s.CloseTime)
	if !closeOK {
		v.Field("close_time", "invalid", "Close time is HH:MM")
	}
	if openOK && closeOK && openM >= closeM {
		v.Field("close_time", "invalid", "Close time must be after open time")
	}
	if s.CapacityPerWindow < 1 {
		v.Field("capacity_per_window", "invalid", "Capacity is at least 1")
	}
	if s.MinLeadMinutes < 0 {
		v.Field("min_lead_minutes", "invalid", "Lead time must not be negative")
	}
	if s.SameDayCutoff != nil {
		if _, ok := parseClock(*s.SameDayCutoff); !ok {
			v.Field("same_day_cutoff", "invalid", "Same-day cut-off is HH:MM")
		}
	}
	for i, d := range s.BlackoutDates {
		if _, err := time.Parse("2006-01-02", d); err != nil {
			v.Field(fmt.Sprintf("blackout_dates[%d]", i), "invalid", "Blackout dates are YYYY-MM-DD")
		}
	}
	if v.HasErrors() {
		return v.Build()
	}
	return nil
}

// SlotID returns a deterministic opaque id for (scheduleID, start UTC).
// Format: "{scheduleUUID}_{unixSeconds}" so list, get and book agree.
func SlotID(scheduleID string, start time.Time) string {
	return fmt.Sprintf("%s_%d", scheduleID, start.UTC().Unix())
}

// ParseSlotID splits a SlotID into schedule UUID and start instant.
func ParseSlotID(id string) (scheduleID string, start time.Time, ok bool) {
	i := strings.LastIndex(id, "_")
	if i <= 0 || i == len(id)-1 {
		return "", time.Time{}, false
	}
	scheduleID, secStr := id[:i], id[i+1:]
	if _, err := uuid.Parse(scheduleID); err != nil {
		return "", time.Time{}, false
	}
	sec, err := strconv.ParseInt(secStr, 10, 64)
	if err != nil {
		return "", time.Time{}, false
	}
	return scheduleID, time.Unix(sec, 0).UTC(), true
}

// GenerateWindows builds candidate slots for [fromDate, toDate] in the
// schedule timezone, applying lead time, same-day cut-off and blackouts.
// Capacity is left unset (RemainingCapacity = CapacityPerWindow); the
// service subtracts bookings. Inactive schedules yield no windows.
func GenerateWindows(s Schedule, fromDate, toDate time.Time, now time.Time) ([]Slot, error) {
	if !s.Active {
		return nil, nil
	}
	tz := s.Timezone
	if tz == "" {
		tz = DefaultScheduleTimezone
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, ycerrors.Validation().Field("timezone", "invalid", "Timezone must be a loadable IANA name").Build()
	}
	openM, ok := parseClock(s.OpenTime)
	if !ok {
		return nil, ycerrors.Validation().Field("open_time", "invalid", "Open time is HH:MM").Build()
	}
	closeM, ok := parseClock(s.CloseTime)
	if !ok {
		return nil, ycerrors.Validation().Field("close_time", "invalid", "Close time is HH:MM").Build()
	}
	var cutoffM *int
	if s.SameDayCutoff != nil {
		m, ok := parseClock(*s.SameDayCutoff)
		if !ok {
			return nil, ycerrors.Validation().Field("same_day_cutoff", "invalid", "Same-day cut-off is HH:MM").Build()
		}
		cutoffM = &m
	}
	weekdayOK := map[time.Weekday]bool{}
	for _, d := range s.Weekdays {
		weekdayOK[time.Weekday(d)] = true
	}
	blackout := map[string]bool{}
	for _, d := range s.BlackoutDates {
		blackout[d] = true
	}

	fromLocal := dateOnly(fromDate.In(loc))
	toLocal := dateOnly(toDate.In(loc))
	nowLocal := now.In(loc)
	earliest := now.Add(time.Duration(s.MinLeadMinutes) * time.Minute)

	var out []Slot
	for day := fromLocal; !day.After(toLocal); day = day.AddDate(0, 0, 1) {
		if !weekdayOK[day.Weekday()] {
			continue
		}
		key := day.Format("2006-01-02")
		if blackout[key] {
			continue
		}
		if cutoffM != nil && sameCalendarDay(day, nowLocal) {
			nowMins := nowLocal.Hour()*60 + nowLocal.Minute()
			if nowMins >= *cutoffM {
				continue
			}
		}
		dayOpen := time.Date(day.Year(), day.Month(), day.Day(), openM/60, openM%60, 0, 0, loc)
		dayClose := time.Date(day.Year(), day.Month(), day.Day(), closeM/60, closeM%60, 0, 0, loc)
		win := time.Duration(s.WindowMinutes) * time.Minute
		for start := dayOpen; !start.Add(win).After(dayClose); start = start.Add(win) {
			end := start.Add(win)
			if start.Before(earliest) {
				continue
			}
			out = append(out, Slot{
				SlotID:            SlotID(s.ID, start),
				ScheduleID:        s.ID,
				StartAt:           start.UTC(),
				EndAt:             end.UTC(),
				Timezone:          tz,
				RemainingCapacity: s.CapacityPerWindow,
			})
		}
	}
	return out, nil
}

func parseClock(hhmm string) (minutes int, ok bool) {
	parts := strings.Split(hhmm, ":")
	if len(parts) != 2 {
		return 0, false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	if len(parts[0]) != 2 || len(parts[1]) != 2 {
		// Accept H:MM as well as HH:MM for admin convenience.
		if len(parts[0]) < 1 || len(parts[0]) > 2 || len(parts[1]) != 2 {
			return 0, false
		}
	}
	return h*60 + m, true
}

func dateOnly(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

func sameCalendarDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}
