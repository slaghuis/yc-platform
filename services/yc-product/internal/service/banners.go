package service

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	"github.com/slaghuis/yc-platform/services/yc-product/internal/domain"
)

// CreateBanner adds a banner for one category, after the shop's other banners.
func (s *Service) CreateBanner(ctx context.Context, in domain.BannerInput) (domain.Banner, error) {
	if err := in.Validate(); err != nil {
		return domain.Banner{}, err
	}
	var b domain.Banner
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		if err := s.bannerCategory(ctx, tx, in); err != nil {
			return err
		}
		var err error
		if b, err = s.cat.CreateBanner(ctx, tx, tenantID, in); err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypeProductBannerCreatedV1, bannerEvent(b, s.now().UTC()))
	})
	return b, err
}

// UpdateBanner replaces a banner's category, text and images.
func (s *Service) UpdateBanner(ctx context.Context, id string, in domain.BannerInput) (domain.Banner, error) {
	if err := in.Validate(); err != nil {
		return domain.Banner{}, err
	}
	var b domain.Banner
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		if _, err := s.cat.GetBanner(ctx, tx, id); err != nil {
			return err
		}
		if err := s.bannerCategory(ctx, tx, in); err != nil {
			return err
		}
		var err error
		if b, err = s.cat.UpdateBanner(ctx, tx, id, in); err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypeProductBannerUpdatedV1, events.ProductBannerUpdatedV1{
			BannerID: b.ID, CategoryID: b.CategoryID, Text: b.Text,
			BackgroundImageID: b.BackgroundImageID, LargeImageID: b.LargeImageID,
			SortOrder: int64(b.SortOrder), UpdatedAt: s.now().UTC(),
		})
	})
	return b, err
}

// DeleteBanner removes a banner from the caller's shop.
func (s *Service) DeleteBanner(ctx context.Context, id string) error {
	return s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		if err := s.cat.DeleteBanner(ctx, tx, id); err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypeProductBannerDeletedV1, events.ProductBannerDeletedV1{
			BannerID: id, DeletedAt: s.now().UTC(),
		})
	})
}

// ReorderBanners sets the shop's display order. ids must list every banner once.
func (s *Service) ReorderBanners(ctx context.Context, ids []string) error {
	if err := domain.ValidateBannerOrder(ids); err != nil {
		return err
	}
	return s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		have, err := s.cat.BannerIDs(ctx, tx)
		if err != nil {
			return err
		}
		if !sameIDs(have, ids) {
			return ycerrors.Validation().Field("ids", "invalid", "List every banner of the shop, once, in the new order").Build()
		}
		if err := s.cat.ReorderBanners(ctx, tx, ids); err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypeProductBannersReorderedV1, events.ProductBannersReorderedV1{
			BannerIds: ids, ReorderedAt: s.now().UTC(),
		})
	})
}

// Banners returns the caller's shop's banners in display order.
func (s *Service) Banners(ctx context.Context) ([]domain.Banner, error) {
	var out []domain.Banner
	err := s.read(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = s.cat.Banners(ctx, tx)
		return err
	})
	return out, err
}

// bannerCategory refuses an unknown category and a large image that is the
// category icon.
func (s *Service) bannerCategory(ctx context.Context, tx pgx.Tx, in domain.BannerInput) error {
	cat, err := s.cat.GetCategory(ctx, tx, in.CategoryID)
	if ycerrors.HasCode(err, ycerrors.CodeNotFound) {
		return ycerrors.Validation().Field("category_id", "unknown", "Unknown category").Build()
	}
	if err != nil {
		return err
	}
	if cat.ImageID != "" && in.LargeImageID == cat.ImageID {
		return ycerrors.Validation().Field("large_image_id", "invalid", "The large image is not the category icon").Build()
	}
	return nil
}

func bannerEvent(b domain.Banner, at time.Time) events.ProductBannerCreatedV1 {
	return events.ProductBannerCreatedV1{
		BannerID: b.ID, CategoryID: b.CategoryID, Text: b.Text,
		BackgroundImageID: b.BackgroundImageID, LargeImageID: b.LargeImageID,
		SortOrder: int64(b.SortOrder), CreatedAt: at,
	}
}

func sameIDs(have, want []string) bool {
	if len(have) != len(want) {
		return false
	}
	seen := make(map[string]int, len(have))
	for _, id := range have {
		seen[id]++
	}
	for _, id := range want {
		if seen[id] == 0 {
			return false
		}
		seen[id]--
	}
	return true
}
