package service

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
)

// RecordCartAdd counts an add to cart (cart.item.added.v1) towards the
// SKU's popularity.
func (s *Service) RecordCartAdd(ctx context.Context, tenantID, skuID string) error {
	if err := validateShopTenant(tenantID); err != nil {
		return err
	}
	return dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		return s.cat.AddPopularity(ctx, tx, skuID, 1)
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
