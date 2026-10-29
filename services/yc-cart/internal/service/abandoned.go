package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	"github.com/slaghuis/yc-platform/services/yc-cart/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-cart/internal/integration"
	"github.com/slaghuis/yc-platform/services/yc-cart/internal/repository"
)

// Abandonment configures abandoned-cart detection and recovery cadence
// (issue #35).
type Abandonment struct {
	// After is how long a customer's cart with lines stays idle before it
	// is announced (default 1h).
	After time.Duration
	// FollowUp is the delay from abandoned_at to touch 2 (default 24h).
	FollowUp time.Duration
	// Final is the delay from abandoned_at to touch 3 (default 72h).
	Final time.Duration
	// BatchSize bounds each sweep.
	BatchSize int
}

// Catalogue enriches recovery reminders with product names and images.
type Catalogue interface {
	SKUs(ctx context.Context, ids []string) (map[string]integration.SKU, error)
	PrimaryImageURL(ctx context.Context, productID string) string
}

// SweepAbandoned publishes cart.abandoned.v1 and the first recovery touch
// for idle customer carts, and returns how many it announced.
func (s *Service) SweepAbandoned(ctx context.Context) (int, error) {
	var types []string
	for _, name := range s.types.Names() {
		if s.types[name].Abandonable {
			types = append(types, name)
		}
	}
	if len(types) == 0 || s.abandon.After <= 0 {
		return 0, nil
	}
	before := s.now().Add(-s.abandon.After)
	var due []repository.Due
	err := dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		due, err = s.repo.IdleCarts(ctx, tx, types, before, s.abandon.BatchSize)
		return err
	})
	if err != nil {
		return 0, err
	}
	var errs []error
	n := 0
	for _, d := range due {
		if ctx.Err() != nil {
			break
		}
		ok, err := s.announceAbandoned(ctx, d, before)
		if ok {
			n++
		}
		errs = append(errs, err)
	}
	return n, errors.Join(errs...)
}

func (s *Service) announceAbandoned(ctx context.Context, d repository.Due, before time.Time) (bool, error) {
	announced := false
	err := dbkit.WithTenantTx(ctx, s.db, d.TenantID, func(tx pgx.Tx) error {
		cart, ok, err := s.repo.LockIdle(ctx, tx, d.TenantID, d.CartID, before)
		if err != nil || !ok || len(cart.Items) == 0 {
			return err
		}
		now := s.now()
		fp := domain.Fingerprint(cart.Items)
		payloadItems := s.recoveryItems(ctx, cart.Items)
		raw, err := json.Marshal(payloadItems)
		if err != nil {
			return err
		}
		next := now.Add(s.abandon.FollowUp)
		if err := s.repo.StartRecovery(ctx, tx, cart.ID, now, fp, raw, 1, &next); err != nil {
			return err
		}
		quantity := int64(0)
		for _, it := range cart.Items {
			quantity += int64(it.Quantity)
		}
		announced = true
		if err := s.publish(ctx, tx, d.TenantID, events.TypeCartAbandonedV1, events.CartAbandonedV1{
			CartID: cart.ID, UserID: cart.UserID, CartType: cart.Type, ItemCount: int64(len(cart.Items)),
			Quantity: quantity, LastActivityAt: cart.UpdatedAt.UTC(), AbandonedAt: now.UTC(),
		}); err != nil {
			return err
		}
		return s.publish(ctx, tx, d.TenantID, events.TypeCartRecoveryTouchedV1, events.CartRecoveryTouchedV1{
			CartID: cart.ID, UserID: cart.UserID, CartType: cart.Type, Touch: 1,
			ItemCount: int64(len(cart.Items)), Quantity: quantity, Items: payloadItems,
			AbandonedAt: now.UTC(), TouchedAt: now.UTC(),
		})
	})
	return announced, err
}

// SweepRecovery sends follow-up recovery touches that are due.
func (s *Service) SweepRecovery(ctx context.Context) (int, error) {
	if s.abandon.FollowUp <= 0 || s.abandon.Final <= 0 {
		return 0, nil
	}
	now := s.now()
	var due []repository.RecoveryDue
	err := dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		due, err = s.repo.DueRecoveries(ctx, tx, now, s.abandon.BatchSize)
		return err
	})
	if err != nil {
		return 0, err
	}
	var errs []error
	n := 0
	for _, d := range due {
		if ctx.Err() != nil {
			break
		}
		ok, err := s.sendRecoveryTouch(ctx, d, now)
		if ok {
			n++
		}
		errs = append(errs, err)
	}
	return n, errors.Join(errs...)
}

func (s *Service) sendRecoveryTouch(ctx context.Context, d repository.RecoveryDue, now time.Time) (bool, error) {
	sent := false
	err := dbkit.WithTenantTx(ctx, s.db, d.TenantID, func(tx pgx.Tx) error {
		cart, fingerprint, snap, abandonedAt, ok, err := s.repo.LockRecovery(ctx, tx, d.TenantID, d.CartID, d.Touch, now)
		if err != nil || !ok {
			return err
		}
		if domain.Fingerprint(cart.Items) != fingerprint || len(cart.Items) == 0 {
			return s.repo.CancelRecovery(ctx, tx, cart.ID)
		}
		nextTouch := d.Touch + 1
		if nextTouch < 2 || nextTouch > 3 {
			return s.repo.CancelRecovery(ctx, tx, cart.ID)
		}
		var nextAt *time.Time
		if nextTouch == 2 {
			t := abandonedAt.Add(s.abandon.Final)
			nextAt = &t
		}
		if err := s.repo.AdvanceRecovery(ctx, tx, cart.ID, nextTouch, nextAt); err != nil {
			return err
		}
		items := snapItems(snap)
		if len(items) == 0 {
			items = s.recoveryItems(ctx, cart.Items)
		}
		quantity := int64(0)
		for _, it := range items {
			quantity += it.Quantity
		}
		sent = true
		return s.publish(ctx, tx, d.TenantID, events.TypeCartRecoveryTouchedV1, events.CartRecoveryTouchedV1{
			CartID: cart.ID, UserID: cart.UserID, CartType: cart.Type, Touch: int64(nextTouch),
			ItemCount: int64(len(items)), Quantity: quantity, Items: items,
			AbandonedAt: abandonedAt.UTC(), TouchedAt: now.UTC(),
		})
	})
	return sent, err
}

func (s *Service) recoveryItems(ctx context.Context, lines []domain.Item) []events.CartRecoveryTouchedV1ItemsItem {
	out := make([]events.CartRecoveryTouchedV1ItemsItem, 0, len(lines))
	ids := make([]string, len(lines))
	for i, it := range lines {
		ids[i] = it.SKUID
		out = append(out, events.CartRecoveryTouchedV1ItemsItem{
			SKUID: it.SKUID, Quantity: int64(it.Quantity),
		})
	}
	if s.catalogue == nil {
		return out
	}
	skus, err := s.catalogue.SKUs(ctx, ids)
	if err != nil {
		telemetry.LoggerFromContext(ctx).Warn("recovery catalogue lookup", "err", err)
		return out
	}
	imageByProduct := map[string]string{}
	for i := range out {
		sku, ok := skus[out[i].SKUID]
		if !ok {
			continue
		}
		if sku.ProductID != "" {
			pid := sku.ProductID
			out[i].ProductID = &pid
		}
		if sku.ProductName != "" {
			name := sku.ProductName
			out[i].Name = &name
		}
		if sku.ProductID == "" {
			continue
		}
		url, ok := imageByProduct[sku.ProductID]
		if !ok {
			url = s.catalogue.PrimaryImageURL(ctx, sku.ProductID)
			imageByProduct[sku.ProductID] = url
		}
		if url != "" {
			u := url
			out[i].ImageURL = &u
		}
	}
	return out
}

func snapItems(raw []byte) []events.CartRecoveryTouchedV1ItemsItem {
	var out []events.CartRecoveryTouchedV1ItemsItem
	if len(raw) == 0 {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	return out
}

// RunAbandonedSweeper sweeps idle carts and due recovery touches until ctx
// is done.
func (s *Service) RunAbandonedSweeper(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := s.SweepAbandoned(ctx); err != nil && ctx.Err() == nil {
			telemetry.LoggerFromContext(ctx).Error("abandoned cart sweep", "err", err)
		}
		if _, err := s.SweepRecovery(ctx); err != nil && ctx.Err() == nil {
			telemetry.LoggerFromContext(ctx).Error("cart recovery sweep", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// AbandonedCarts lists the shop's carts announced as abandoned and not
// used since, most recently abandoned first.
func (s *Service) AbandonedCarts(ctx context.Context, after *repository.AbandonedCursor, limit int) ([]repository.Abandoned, *repository.AbandonedCursor, error) {
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorShopAdmin {
		return nil, nil, ycerrors.Forbidden("Shop admins only").WithInternal("actor_type %s", a.ActorType)
	}
	if err := validateShopTenant(a.TenantID); err != nil {
		return nil, nil, err
	}
	var rows []repository.Abandoned
	err := dbkit.WithTenantTx(ctx, s.db, a.TenantID, func(tx pgx.Tx) error {
		var err error
		rows, err = s.repo.AbandonedCarts(ctx, tx, a.TenantID, after, limit+1)
		return err
	})
	if err != nil || len(rows) <= limit {
		return rows, nil, err
	}
	rows = rows[:limit]
	last := rows[limit-1]
	return rows, &repository.AbandonedCursor{AbandonedAt: last.AbandonedAt, ID: last.CartID}, nil
}
