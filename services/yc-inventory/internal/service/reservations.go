package service

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	"github.com/slaghuis/yc-platform/services/yc-inventory/internal/domain"
)

// ReleaseReason says why a reservation was released.
type ReleaseReason string

// Release reasons (inventory.stock.reservation_released.v1).
const (
	ReleaseCompensation ReleaseReason = "compensation"
	ReleaseAdmin        ReleaseReason = "admin"
)

// ReserveRequest is yc-checkout's RESERVING call.
type ReserveRequest struct {
	SessionID      string
	IdempotencyKey string
	Items          []domain.Line
	TTLMinutes     *int
}

// Reserve holds the tracked lines of a checkout session, all or nothing
// (ADR-012). Untracked SKUs, and every SKU of a shop with inventory turned
// off, need no stock: when nothing is tracked there is no reservation
// (nil). A repeated idempotency key returns the reservation it made, in
// its current state. A shortage is insufficient_stock with the shortages.
func (s *Service) Reserve(ctx context.Context, req ReserveRequest) (*domain.Reservation, error) {
	if _, err := uuid.Parse(req.SessionID); err != nil {
		return nil, ycerrors.Validation().Field("session_id", "invalid", "Session ID is not a UUID").Build()
	}
	if req.IdempotencyKey == "" {
		return nil, ycerrors.Validation().Field("Idempotency-Key", "required", "Idempotency-Key header is required").Build()
	}
	lines, err := domain.NormaliseLines(req.Items)
	if err != nil {
		return nil, err
	}
	var (
		res       *domain.Reservation
		shortages []domain.Shortage
	)
	err = s.asService(ctx, func(tx pgx.Tx, tenantID string) error {
		if r, ok, err := s.repo.ReservationByKey(ctx, tx, tenantID, req.IdempotencyKey); err != nil || ok {
			if ok {
				res = &r
			}
			return err
		}
		cfg, err := s.config(ctx, tx, tenantID)
		if err != nil || !cfg.Enabled {
			return err
		}
		skus := make([]string, len(lines))
		for i, l := range lines {
			skus[i] = l.SKUID
		}
		levels, err := s.repo.LockLevels(ctx, tx, tenantID, skus)
		if err != nil {
			return err
		}
		var tracked []domain.Line
		for _, l := range lines {
			lv, ok := levels[l.SKUID]
			if !ok {
				continue
			}
			tracked = append(tracked, l)
			if lv.Available() < l.Quantity {
				shortages = append(shortages, domain.Shortage{SKUID: l.SKUID, Requested: l.Quantity, Available: max(lv.Available(), 0)})
			}
		}
		now := s.now()
		if len(shortages) > 0 {
			return s.publishShortages(ctx, tx, tenantID, req.SessionID, shortages, now)
		}
		if len(tracked) == 0 {
			return nil
		}
		r := domain.Reservation{
			ID: uuid.NewString(), SessionID: req.SessionID, IdempotencyKey: req.IdempotencyKey,
			Status: domain.ReservationActive, ExpiresAt: now.Add(cfg.TTL(req.TTLMinutes)), CreatedAt: now, Items: tracked,
		}
		if err := s.repo.InsertReservation(ctx, tx, tenantID, r); err != nil {
			return err
		}
		items := make([]events.InventoryStockReservedV1ItemsItem, len(tracked))
		for i, l := range tracked {
			items[i] = events.InventoryStockReservedV1ItemsItem{SKUID: l.SKUID, Quantity: l.Quantity}
			before := levels[l.SKUID]
			err := s.applyLine(ctx, tx, tenantID, before, before.Reserve(l.Quantity), domain.Movement{
				Type: domain.MovementReserve, ReservedDelta: l.Quantity, Reference: r.ID,
			}, now)
			if err != nil {
				return err
			}
		}
		res = &r
		return s.publish(ctx, tx, tenantID, events.TypeInventoryStockReservedV1, events.InventoryStockReservedV1{
			ReservationID: r.ID, SessionID: r.SessionID, Items: items, ExpiresAt: r.ExpiresAt.UTC(), ReservedAt: now.UTC(),
		})
	})
	if err != nil {
		return nil, err
	}
	if len(shortages) > 0 {
		return nil, ycerrors.New(ycerrors.CodeInsufficientStock, "Some items are not available in the quantity requested").
			With("shortages", shortages)
	}
	return res, nil
}

func (s *Service) publishShortages(ctx context.Context, tx pgx.Tx, tenantID, sessionID string, shortages []domain.Shortage, now time.Time) error {
	items := make([]events.InventoryStockReservationFailedV1ShortagesItem, len(shortages))
	for i, sh := range shortages {
		items[i] = events.InventoryStockReservationFailedV1ShortagesItem{SKUID: sh.SKUID, Requested: sh.Requested, Available: sh.Available}
	}
	return s.publish(ctx, tx, tenantID, events.TypeInventoryStockReservationFailedV1, events.InventoryStockReservationFailedV1{
		SessionID: sessionID, Shortages: items, FailedAt: now.UTC(),
	})
}

// CommitResult is the outcome of Commit.
type CommitResult struct {
	Reservation domain.Reservation
	Oversold    []domain.Line
}

// Commit takes a reservation's stock out of on_hand once the order is paid
// (CONFIRMING). Committing again returns the reservation. A reservation
// that expired meanwhile still commits; quantities no longer available are
// reported as oversold. A released reservation cannot be committed.
func (s *Service) Commit(ctx context.Context, id, reference string) (CommitResult, error) {
	var out CommitResult
	err := s.asService(ctx, func(tx pgx.Tx, tenantID string) error {
		r, err := s.reservation(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		out.Reservation = r
		switch r.Status {
		case domain.ReservationCommitted:
			return nil
		case domain.ReservationReleased:
			return ycerrors.New(ycerrors.CodeInvalidStateTransition, "The reservation was released")
		case domain.ReservationActive, domain.ReservationExpired:
		}
		levels, err := s.lockItems(ctx, tx, tenantID, r)
		if err != nil {
			return err
		}
		now := s.now()
		active := r.Status == domain.ReservationActive
		items := make([]events.InventoryStockCommittedV1ItemsItem, len(r.Items))
		var oversold []events.InventoryStockCommittedV1OversoldItem
		for i, l := range r.Items {
			items[i] = events.InventoryStockCommittedV1ItemsItem{SKUID: l.SKUID, Quantity: l.Quantity}
			before, ok := levels[l.SKUID]
			if !ok {
				continue // no longer tracked
			}
			after, over := before.Commit(l.Quantity, active)
			if over > 0 {
				out.Oversold = append(out.Oversold, domain.Line{SKUID: l.SKUID, Quantity: over})
				oversold = append(oversold, events.InventoryStockCommittedV1OversoldItem{SKUID: l.SKUID, Quantity: over})
			}
			m := domain.Movement{
				Type: domain.MovementCommit, OnHandDelta: after.OnHand - before.OnHand,
				ReservedDelta: after.Reserved - before.Reserved, Reference: firstNonEmpty(reference, r.ID),
			}
			if err := s.applyLine(ctx, tx, tenantID, before, after, m, now); err != nil {
				return err
			}
		}
		if err := s.repo.SettleReservation(ctx, tx, tenantID, r.ID, domain.ReservationCommitted, now); err != nil {
			return err
		}
		out.Reservation.Status, out.Reservation.SettledAt = domain.ReservationCommitted, &now
		return s.publish(ctx, tx, tenantID, events.TypeInventoryStockCommittedV1, events.InventoryStockCommittedV1{
			ReservationID: r.ID, SessionID: r.SessionID, Items: items, Oversold: oversold, CommittedAt: now.UTC(),
		})
	})
	return out, err
}

// Release returns an active reservation's stock (compensation). Releasing
// a reservation that is already released, expired or unknown is a no-op
// (ok false for unknown); a committed one cannot be released.
func (s *Service) Release(ctx context.Context, id string) (domain.Reservation, bool, error) {
	var (
		r     domain.Reservation
		found bool
	)
	err := s.asService(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		r, found, err = s.repo.Reservation(ctx, tx, tenantID, id, true)
		if err != nil || !found {
			return err
		}
		r, err = s.release(ctx, tx, tenantID, r, ReleaseCompensation)
		return err
	})
	return r, found, err
}

// AdminRelease releases an active reservation by hand.
func (s *Service) AdminRelease(ctx context.Context, id string) (domain.Reservation, error) {
	var r domain.Reservation
	err := s.asShopAdmin(ctx, func(tx pgx.Tx, tenantID string) error {
		var err error
		if r, err = s.reservation(ctx, tx, tenantID, id); err != nil {
			return err
		}
		if r.Status != domain.ReservationActive {
			return ycerrors.New(ycerrors.CodeInvalidStateTransition, "Only active reservations can be released")
		}
		r, err = s.release(ctx, tx, tenantID, r, ReleaseAdmin)
		return err
	})
	return r, err
}

func (s *Service) release(ctx context.Context, tx pgx.Tx, tenantID string, r domain.Reservation, reason ReleaseReason) (domain.Reservation, error) {
	switch r.Status {
	case domain.ReservationReleased, domain.ReservationExpired:
		return r, nil
	case domain.ReservationCommitted:
		return r, ycerrors.New(ycerrors.CodeInvalidStateTransition, "The reservation was committed")
	case domain.ReservationActive:
	}
	now := s.now()
	items, err := s.returnStock(ctx, tx, tenantID, r, domain.MovementRelease, now)
	if err != nil {
		return r, err
	}
	if err := s.repo.SettleReservation(ctx, tx, tenantID, r.ID, domain.ReservationReleased, now); err != nil {
		return r, err
	}
	r.Status, r.SettledAt = domain.ReservationReleased, &now
	evtItems := make([]events.InventoryStockReservationReleasedV1ItemsItem, len(items))
	for i, l := range items {
		evtItems[i] = events.InventoryStockReservationReleasedV1ItemsItem{SKUID: l.SKUID, Quantity: l.Quantity}
	}
	return r, s.publish(ctx, tx, tenantID, events.TypeInventoryStockReservationReleasedV1, events.InventoryStockReservationReleasedV1{
		ReservationID: r.ID, SessionID: r.SessionID, Items: evtItems, Reason: string(reason), ReleasedAt: now.UTC(),
	})
}

// Expire ends one reservation past its expiry, if it is still active.
func (s *Service) Expire(ctx context.Context, tenantID, id string) (bool, error) {
	expired := false
	err := dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		r, ok, err := s.repo.Reservation(ctx, tx, tenantID, id, true)
		now := s.now()
		if err != nil || !ok || r.Status != domain.ReservationActive || r.ExpiresAt.After(now) {
			return err
		}
		items, err := s.returnStock(ctx, tx, tenantID, r, domain.MovementExpire, now)
		if err != nil {
			return err
		}
		if err := s.repo.SettleReservation(ctx, tx, tenantID, r.ID, domain.ReservationExpired, now); err != nil {
			return err
		}
		expired = true
		evtItems := make([]events.InventoryStockReservationExpiredV1ItemsItem, len(items))
		for i, l := range items {
			evtItems[i] = events.InventoryStockReservationExpiredV1ItemsItem{SKUID: l.SKUID, Quantity: l.Quantity}
		}
		return s.publish(ctx, tx, tenantID, events.TypeInventoryStockReservationExpiredV1, events.InventoryStockReservationExpiredV1{
			ReservationID: r.ID, SessionID: r.SessionID, Items: evtItems, ExpiredAt: now.UTC(),
		})
	})
	return expired, err
}

// returnStock gives an active reservation's quantities back to available.
func (s *Service) returnStock(ctx context.Context, tx pgx.Tx, tenantID string, r domain.Reservation, mt domain.MovementType, now time.Time) ([]domain.Line, error) {
	levels, err := s.lockItems(ctx, tx, tenantID, r)
	if err != nil {
		return nil, err
	}
	for _, l := range r.Items {
		before, ok := levels[l.SKUID]
		if !ok {
			continue
		}
		after := before.Release(l.Quantity)
		m := domain.Movement{Type: mt, ReservedDelta: after.Reserved - before.Reserved, Reference: r.ID}
		if err := s.applyLine(ctx, tx, tenantID, before, after, m, now); err != nil {
			return nil, err
		}
	}
	return r.Items, nil
}

func (s *Service) lockItems(ctx context.Context, tx pgx.Tx, tenantID string, r domain.Reservation) (map[string]domain.Level, error) {
	skus := make([]string, len(r.Items))
	for i, l := range r.Items {
		skus[i] = l.SKUID
	}
	return s.repo.LockLevels(ctx, tx, tenantID, skus)
}

// applyLine writes one level change of a reservation with its movement and
// signals.
func (s *Service) applyLine(ctx context.Context, tx pgx.Tx, tenantID string, before, after domain.Level, m domain.Movement, now time.Time) error {
	if err := s.repo.UpdateLevel(ctx, tx, tenantID, after, now); err != nil {
		return err
	}
	a, _ := authctx.FromContext(ctx)
	m.SKUID, m.ActorID, m.CreatedAt = after.SKUID, a.ActorID, now
	if err := s.repo.AddMovement(ctx, tx, tenantID, m); err != nil {
		return err
	}
	return s.publishSignals(ctx, tx, tenantID, before, after, now)
}

func (s *Service) reservation(ctx context.Context, tx pgx.Tx, tenantID, id string) (domain.Reservation, error) {
	r, ok, err := s.repo.Reservation(ctx, tx, tenantID, id, true)
	if err != nil {
		return domain.Reservation{}, err
	}
	if !ok {
		return domain.Reservation{}, ycerrors.NotFound("reservation", id)
	}
	return r, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
