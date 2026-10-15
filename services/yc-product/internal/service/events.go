package service

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	"github.com/slaghuis/yc-platform/services/yc-product/internal/domain"
)

// serviceName attributes the events yc-product publishes on its own behalf.
const serviceName = "yc-product"

// RecordOrder counts a placed order (order.placed.v1) towards its SKUs'
// popularity and, for a customer's order, the customer's favourites.
// Visitors' orders count towards popularity only.
func (s *Service) RecordOrder(ctx context.Context, tenantID, userID string, customer bool, skuIDs []string, at time.Time) error {
	if err := validateShopTenant(tenantID); err != nil {
		return err
	}
	if !customer {
		userID = ""
	}
	return dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		return s.cat.RecordOrder(ctx, tx, tenantID, userID, skuIDs, at)
	})
}

// ForgetUser deletes a deleted customer's favourites (ADR-005) and reports
// yc-product's part of the cascade. Popularity counts no one and stays.
func (s *Service) ForgetUser(ctx context.Context, tenantID string, e events.IdentityUserDeletedV1) error {
	if err := validateShopTenant(tenantID); err != nil {
		return err
	}
	return dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		n, err := s.cat.DeleteFavourites(ctx, tx, tenantID, e.UserID)
		if err != nil {
			return err
		}
		return s.bus.Publish(ctx, tx, events.GDPRCompleted(tenantID, serviceName, e, events.GDPRDeleted, n, s.now()))
	})
}

// SetReviewStats records a product's review stats as carried by review.*
// events, ignoring ones older than those recorded.
func (s *Service) SetReviewStats(ctx context.Context, tenantID, productID string, stats domain.ReviewStats) error {
	if err := validateShopTenant(tenantID); err != nil {
		return err
	}
	if err := stats.Validate(); err != nil {
		return err
	}
	return dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		return s.cat.SetReviewStats(ctx, tx, tenantID, productID, stats)
	})
}

// SetInStock records whether a tracked SKU has stock
// (inventory.stock.depleted.v1, inventory.stock.restored.v1).
func (s *Service) SetInStock(ctx context.Context, tenantID, skuID string, inStock bool, at time.Time) error {
	if err := validateShopTenant(tenantID); err != nil {
		return err
	}
	return dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		return s.cat.SetInStock(ctx, tx, skuID, inStock, at)
	})
}

// SetBasePrice records a SKU's base price in minor units of the store
// currency (pricing.base_price.set.v1), or its removal when minor is nil
// (pricing.base_price.removed.v1).
func (s *Service) SetBasePrice(ctx context.Context, tenantID, skuID string, minor *int64, at time.Time) error {
	if err := validateShopTenant(tenantID); err != nil {
		return err
	}
	return dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		return s.cat.SetBasePrice(ctx, tx, skuID, minor, at)
	})
}
