package service

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	"github.com/slaghuis/yc-platform/services/yc-pricing/internal/domain"
)

// InitShop gives a newly provisioned shop its pricing configuration. An
// existing configuration is kept, so redelivery is harmless.
func (s *Service) InitShop(ctx context.Context, tenantID string, in domain.ConfigInput) error {
	if err := validateShopTenant(tenantID); err != nil {
		return err
	}
	if err := in.Validate(); err != nil {
		return err
	}
	return dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		created, err := s.repo.InitConfig(ctx, tx, tenantID, in, s.now())
		if err != nil || !created {
			return err
		}
		c, err := s.repo.Config(ctx, tx, tenantID, false)
		if err != nil {
			return err
		}
		return s.publishConfig(ctx, tx, c)
	})
}

// SyncSKU records a SKU's product and tax class from product.sku.created/updated.
func (s *Service) SyncSKU(ctx context.Context, tenantID, skuID, productID, taxClass, status string, at time.Time) error {
	return dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		return s.repo.SyncSKU(ctx, tx, tenantID, skuID, productID, taxClass, status, at)
	})
}

// DeleteSKUs forgets deleted SKUs and removes their base prices.
func (s *Service) DeleteSKUs(ctx context.Context, tenantID string, skuIDs []string, at time.Time) error {
	return dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		for _, id := range skuIDs {
			if err := s.repo.DeleteSKU(ctx, tx, tenantID, id, at); err != nil {
				return err
			}
			if err := s.removePrice(ctx, tx, tenantID, id, false); err != nil {
				return err
			}
		}
		return nil
	})
}
