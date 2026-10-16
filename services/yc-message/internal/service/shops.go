package service

import (
	"context"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"

	events "github.com/slaghuis/yc-platform/libs/yc-events"
	"github.com/slaghuis/yc-platform/services/yc-message/internal/domain"
)

// TenantProvisioned records the shop's name and primary hostname so links
// use the real storefront host (ADR-029, ADR-027 rule 14).
func (s *Service) TenantProvisioned(ctx context.Context, _ string, p events.PlatformTenantProvisionedV1) error {
	primary := ""
	if len(p.Hostnames) > 0 {
		primary = p.Hostnames[0]
	}
	if primary == "" {
		return nil
	}
	return s.inTenant(ctx, p.TenantID, func(tx pgx.Tx, tenantID string) error {
		return s.repo.PutShop(ctx, tx, tenantID, p.Name, primary, p.ProvisionedAt)
	})
}

// TenantUpdated refreshes the shop's name and primary hostname.
func (s *Service) TenantUpdated(ctx context.Context, _ string, p events.PlatformTenantUpdatedV1) error {
	primary := p.PrimaryHostname
	if primary == "" && len(p.Hostnames) > 0 {
		primary = p.Hostnames[0]
	}
	if primary == "" {
		return nil
	}
	return s.inTenant(ctx, p.TenantID, func(tx pgx.Tx, tenantID string) error {
		return s.repo.PutShop(ctx, tx, tenantID, p.Name, primary, p.UpdatedAt)
	})
}

// TenantDeleted clears the shop projection so links fall back to the
// configured shop_url template until a new shop reuses the hostnames.
func (s *Service) TenantDeleted(ctx context.Context, _ string, p events.PlatformTenantDeletedV1) error {
	return s.inTenant(ctx, p.TenantID, func(tx pgx.Tx, tenantID string) error {
		return s.repo.DeleteShop(ctx, tx, tenantID)
	})
}

// links returns storefront links for a shop. When the projection has the
// primary hostname, that host is used (keeping the scheme and port from
// messages.shop_url); otherwise {tenant} in the template is replaced.
func (s *Service) links(ctx context.Context, tenantID string) domain.ShopLinks {
	base := strings.ReplaceAll(s.cfg.ShopURL, "{tenant}", tenantID)
	_ = s.inTenant(ctx, tenantID, func(tx pgx.Tx, tenantID string) error {
		host, ok, err := s.repo.ShopPrimaryHost(ctx, tx, tenantID)
		if err != nil || !ok || host == "" {
			return err
		}
		base = shopBaseURL(s.cfg.ShopURL, host)
		return nil
	})
	return domain.NewShopLinks(base)
}

// shopBaseURL builds a storefront base from the configured template's
// scheme and port, with hostname as the host.
func shopBaseURL(template, hostname string) string {
	u, err := url.Parse(strings.ReplaceAll(template, "{tenant}", "x"))
	if err != nil || u.Scheme == "" {
		return "https://" + hostname
	}
	host := hostname
	if port := u.Port(); port != "" && !strings.Contains(hostname, ":") {
		host = hostname + ":" + port
	}
	u.Host = host
	u.Path, u.RawQuery, u.Fragment = "", "", ""
	return strings.TrimSuffix(u.String(), "/")
}
