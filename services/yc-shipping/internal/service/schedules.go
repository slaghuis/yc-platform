package service

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	"github.com/slaghuis/yc-platform/services/yc-shipping/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-shipping/internal/repository"
)

// Schedules returns the shop's delivery schedules.
func (s *Service) Schedules(ctx context.Context) ([]domain.Schedule, error) {
	var out []domain.Schedule
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		out, err = s.repo.Schedules(ctx, tx, tenantID)
		return err
	})
	return out, err
}

// Schedule returns one schedule by ID.
func (s *Service) Schedule(ctx context.Context, id string) (domain.Schedule, error) {
	var out domain.Schedule
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		sc, ok, err := s.repo.Schedule(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if !ok {
			return ycerrors.NotFound("schedule", id)
		}
		out = sc
		return nil
	})
	return out, err
}

// CreateSchedule adds a delivery schedule.
func (s *Service) CreateSchedule(ctx context.Context, sc domain.Schedule) (domain.Schedule, error) {
	sc.Normalise()
	if err := sc.Validate(); err != nil {
		return domain.Schedule{}, err
	}
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		now := s.now().UTC()
		sc.ID, sc.CreatedAt, sc.UpdatedAt = uuid.NewString(), now, now
		return s.repo.PutSchedule(ctx, tx, tenantID, sc)
	})
	return sc, err
}

// UpdateSchedule replaces a delivery schedule.
func (s *Service) UpdateSchedule(ctx context.Context, id string, sc domain.Schedule) (domain.Schedule, error) {
	sc.Normalise()
	if err := sc.Validate(); err != nil {
		return domain.Schedule{}, err
	}
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		old, ok, err := s.repo.Schedule(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if !ok {
			return ycerrors.NotFound("schedule", id)
		}
		sc.ID, sc.CreatedAt, sc.UpdatedAt = id, old.CreatedAt, s.now().UTC()
		return s.repo.PutSchedule(ctx, tx, tenantID, sc)
	})
	return sc, err
}

// DeleteSchedule removes a schedule that no rate references.
func (s *Service) DeleteSchedule(ctx context.Context, id string) error {
	return s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		used, err := s.repo.RatesReferencingSchedule(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		if used {
			return ycerrors.Conflict("A rate still uses this schedule")
		}
		ok, err := s.repo.DeleteSchedule(ctx, tx, tenantID, id)
		if err == nil && !ok {
			err = ycerrors.NotFound("schedule", id)
		}
		return err
	})
}

// ListSlots returns available windows for a rate in [from, to] (max 14 days).
func (s *Service) ListSlots(ctx context.Context, rateID string, from, to time.Time) ([]domain.Slot, error) {
	if err := validateSlotRange(from, to); err != nil {
		return nil, err
	}
	var out []domain.Slot
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		rt, ok, err := s.repo.Rate(ctx, tx, tenantID, rateID)
		if err != nil {
			return err
		}
		if !ok || rt.ScheduleID == "" {
			out = []domain.Slot{}
			return nil
		}
		sc, ok, err := s.repo.Schedule(ctx, tx, tenantID, rt.ScheduleID)
		if err != nil {
			return err
		}
		if !ok || !sc.Active {
			out = []domain.Slot{}
			return nil
		}
		windows, err := domain.GenerateWindows(sc, from, to, s.now())
		if err != nil {
			return err
		}
		starts := make([]time.Time, len(windows))
		for i, w := range windows {
			starts[i] = w.StartAt
		}
		counts, err := s.repo.BookingCounts(ctx, tx, tenantID, sc.ID, starts)
		if err != nil {
			return err
		}
		out = make([]domain.Slot, 0, len(windows))
		for _, w := range windows {
			remaining := sc.CapacityPerWindow - counts[w.StartAt.Unix()]
			if remaining <= 0 {
				continue
			}
			w.RemainingCapacity = remaining
			out = append(out, w)
		}
		return nil
	})
	return out, err
}

// GetSlot returns one slot when it still has remaining capacity, else 404.
func (s *Service) GetSlot(ctx context.Context, slotID string) (domain.Slot, error) {
	scheduleID, start, ok := domain.ParseSlotID(slotID)
	if !ok {
		return domain.Slot{}, ycerrors.NotFound("slot", slotID)
	}
	var out domain.Slot
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		sc, found, err := s.repo.Schedule(ctx, tx, tenantID, scheduleID)
		if err != nil {
			return err
		}
		if !found || !sc.Active {
			return ycerrors.NotFound("slot", slotID)
		}
		slot, err := s.availableSlot(ctx, tx, tenantID, sc, start)
		if err != nil {
			return err
		}
		if slot == nil {
			return ycerrors.NotFound("slot", slotID)
		}
		out = *slot
		return nil
	})
	return out, err
}

// BookSlot hard-books capacity for bookingKey (checkout_session_id). Idempotent
// for the same key and slot; 409 when full or the key holds a different slot.
func (s *Service) BookSlot(ctx context.Context, slotID, bookingKey string) (domain.Slot, error) {
	bookingKey = strings.TrimSpace(bookingKey)
	if bookingKey == "" {
		return domain.Slot{}, ycerrors.Validation().Field("booking_key", "required", "Booking key is required").Build()
	}
	scheduleID, start, ok := domain.ParseSlotID(slotID)
	if !ok {
		return domain.Slot{}, ycerrors.NotFound("slot", slotID)
	}
	var out domain.Slot
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		sc, found, err := s.repo.Schedule(ctx, tx, tenantID, scheduleID)
		if err != nil {
			return err
		}
		if !found || !sc.Active {
			return ycerrors.NotFound("slot", slotID)
		}
		slot, err := s.availableSlot(ctx, tx, tenantID, sc, start)
		if err != nil {
			return err
		}
		if slot == nil {
			return ycerrors.Conflict("This delivery slot is no longer available")
		}
		existing, has, err := s.repo.BookingByKey(ctx, tx, tenantID, bookingKey)
		if err != nil {
			return err
		}
		if has {
			if existing.ScheduleID == scheduleID && existing.SlotStart.UTC().Equal(start.UTC()) {
				out = *slot
				return nil
			}
			return ycerrors.Conflict("This booking key already holds another delivery slot")
		}
		ok, err := s.repo.BookSlot(ctx, tx, tenantID, repository.SlotBooking{
			ScheduleID: scheduleID,
			SlotStart:  slot.StartAt,
			SlotEnd:    slot.EndAt,
			BookingKey: bookingKey,
			CreatedAt:  s.now().UTC(),
		}, sc.CapacityPerWindow)
		if err != nil {
			return err
		}
		if !ok {
			return ycerrors.Conflict("This delivery slot is full")
		}
		n, err := s.repo.BookingCount(ctx, tx, tenantID, scheduleID, slot.StartAt)
		if err != nil {
			return err
		}
		slot.RemainingCapacity = sc.CapacityPerWindow - n
		out = *slot
		return nil
	})
	return out, err
}

// ReleaseSlot frees capacity for bookingKey. slotID is optional; when set,
// only that window is released.
func (s *Service) ReleaseSlot(ctx context.Context, slotID, bookingKey string) error {
	bookingKey = strings.TrimSpace(bookingKey)
	if bookingKey == "" {
		return ycerrors.Validation().Field("booking_key", "required", "Booking key is required").Build()
	}
	var start *time.Time
	if slotID != "" {
		_, t, ok := domain.ParseSlotID(slotID)
		if !ok {
			return ycerrors.NotFound("slot", slotID)
		}
		start = &t
	}
	return s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		_, err := s.repo.ReleaseByKey(ctx, tx, tenantID, bookingKey, start)
		return err
	})
}

func (s *Service) availableSlot(ctx context.Context, tx pgx.Tx, tenantID string, sc domain.Schedule, start time.Time) (*domain.Slot, error) {
	tz := sc.Timezone
	if tz == "" {
		tz = domain.DefaultScheduleTimezone
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, err
	}
	day := start.In(loc)
	windows, err := domain.GenerateWindows(sc, day, day, s.now())
	if err != nil {
		return nil, err
	}
	var match *domain.Slot
	for i := range windows {
		if windows[i].StartAt.UTC().Equal(start.UTC()) {
			match = &windows[i]
			break
		}
	}
	if match == nil {
		return nil, nil
	}
	n, err := s.repo.BookingCount(ctx, tx, tenantID, sc.ID, start)
	if err != nil {
		return nil, err
	}
	remaining := sc.CapacityPerWindow - n
	if remaining <= 0 {
		return nil, nil
	}
	match.RemainingCapacity = remaining
	return match, nil
}

func validateSlotRange(from, to time.Time) error {
	v := ycerrors.Validation()
	if from.IsZero() {
		v.Field("from", "required", "From date is required")
	}
	if to.IsZero() {
		v.Field("to", "required", "To date is required")
	}
	if !from.IsZero() && !to.IsZero() {
		if to.Before(from) {
			v.Field("to", "invalid", "To must not be before from")
		} else if to.Sub(from) > time.Duration(domain.MaxSlotListDays)*24*time.Hour {
			v.Field("to", "invalid", "List range is at most 14 days")
		}
	}
	if v.HasErrors() {
		return v.Build()
	}
	return nil
}
