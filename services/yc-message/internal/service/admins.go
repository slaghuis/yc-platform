package service

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	money "github.com/slaghuis/yc-platform/libs/yc-money"
	"github.com/slaghuis/yc-platform/services/yc-message/internal/domain"
)

// Alert audiences (monitor.alert.opened.v1 notify).
const (
	notifySystemAdmin = "system_admin"
	notifyShopAdmin   = "shop_admin"
)

// ShopAdminPromoted keeps the shop's admins, whom alerts and milestones go
// to, in step with yc-identity.
func (s *Service) ShopAdminPromoted(ctx context.Context, tenantID, _ string, p events.IdentityUserPromotedV1) error {
	return s.inTenant(ctx, tenantID, func(tx pgx.Tx, tenantID string) error {
		if p.ToRole == notifyShopAdmin {
			return s.repo.PutShopAdmin(ctx, tx, tenantID, p.UserID, p.PromotedAt)
		}
		return s.repo.DeleteShopAdmin(ctx, tx, tenantID, p.UserID)
	})
}

// InvitationAccepted adds the new admin to the alert audience (ADR-030).
func (s *Service) InvitationAccepted(ctx context.Context, _, _ string, p events.IdentityUserInvitationAcceptedV1) error {
	switch p.Role {
	case notifyShopAdmin:
		return s.inTenant(ctx, p.TenantID, func(tx pgx.Tx, tenantID string) error {
			return s.repo.PutShopAdmin(ctx, tx, tenantID, p.UserID, p.AcceptedAt)
		})
	case notifySystemAdmin:
		return s.inAnyTenant(ctx, eventbus.PlatformTenantID, func(tx pgx.Tx, _ string) error {
			return s.repo.PutSystemAdmin(ctx, tx, p.UserID, p.Email, p.AcceptedAt)
		})
	default:
		return nil
	}
}

// UserDemoted removes a shop admin from the alert audience (ADR-030).
func (s *Service) UserDemoted(ctx context.Context, tenantID, _ string, p events.IdentityUserDemotedV1) error {
	return s.inTenant(ctx, tenantID, func(tx pgx.Tx, tenantID string) error {
		return s.repo.DeleteShopAdmin(ctx, tx, tenantID, p.UserID)
	})
}

// UserSuspended removes the user from shop and system alert audiences (ADR-030).
func (s *Service) UserSuspended(ctx context.Context, _, _ string, p events.IdentityUserSuspendedV1) error {
	return s.inAnyTenant(ctx, eventbus.PlatformTenantID, func(tx pgx.Tx, _ string) error {
		if err := s.repo.SuspendSystemAdmin(ctx, tx, p.UserID, p.SuspendedAt); err != nil {
			return err
		}
		if p.TenantID == eventbus.PlatformTenantID || p.TenantID == "" {
			return nil
		}
		return s.repo.DeleteShopAdmin(ctx, tx, p.TenantID, p.UserID)
	})
}

// UserReactivated restores admin alert audiences for shop_admin and
// system_admin roles (ADR-030).
func (s *Service) UserReactivated(ctx context.Context, _, _ string, p events.IdentityUserReactivatedV1) error {
	switch p.Role {
	case notifyShopAdmin:
		return s.inTenant(ctx, p.TenantID, func(tx pgx.Tx, tenantID string) error {
			return s.repo.PutShopAdmin(ctx, tx, tenantID, p.UserID, p.ReactivatedAt)
		})
	case notifySystemAdmin:
		return s.inAnyTenant(ctx, eventbus.PlatformTenantID, func(tx pgx.Tx, _ string) error {
			return s.repo.ReactivateSystemAdmin(ctx, tx, p.UserID)
		})
	default:
		return nil
	}
}

// AdminAlert emails a yc-monitor alert to the admins it names: the
// platform's system admins (from configuration and invited) and the
// alert's shop's admins. Platform alerts are kept under the platform tenant.
func (s *Service) AdminAlert(ctx context.Context, tenantID, eventID string, p events.MonitorAlertOpenedV1) error {
	dims := make([]map[string]any, 0, len(p.Dimensions))
	for _, k := range slices.Sorted(maps.Keys(p.Dimensions)) {
		dims = append(dims, map[string]any{"name": k, "value": p.Dimensions[k]})
	}
	msgCtx := map[string]any{
		"severity": p.Severity, "summary": p.Summary, "rule": p.Rule, "metric": p.Metric, "dimensions": dims,
		"value": number(p.Value), "threshold": number(p.Threshold), "opened_at": stamp(p.OpenedAt),
	}
	var to []domain.Recipient
	if slices.Contains(p.Notify, notifySystemAdmin) {
		seen := map[string]bool{}
		for _, email := range s.cfg.SystemAdminEmails {
			if seen[email] {
				continue
			}
			seen[email] = true
			to = append(to, domain.Recipient{Channel: domain.ChannelEmail, Address: email})
		}
		invited, err := s.invitedSystemAdminEmails(ctx)
		if err != nil {
			return err
		}
		for _, email := range invited {
			if seen[email] {
				continue
			}
			seen[email] = true
			to = append(to, domain.Recipient{Channel: domain.ChannelEmail, Address: email})
		}
	}
	if slices.Contains(p.Notify, notifyShopAdmin) && tenantID != eventbus.PlatformTenantID {
		admins, err := s.shopAdmins(ctx, tenantID)
		if err != nil {
			return err
		}
		to = append(to, admins...)
	}
	return s.notifyAll(ctx, tenantID, eventID, domain.TypeAdminAlert, to, msgCtx)
}

// Milestone congratulates the shop's admins.
func (s *Service) Milestone(ctx context.Context, tenantID, eventID string, p events.AnalyticsMilestoneReachedV1) error {
	to, err := s.shopAdmins(ctx, tenantID)
	if err != nil {
		return err
	}
	return s.notifyAll(ctx, tenantID, eventID, domain.TypeMilestone, to, map[string]any{
		"milestone": p.Milestone, "title": milestoneTitle(p), "reached_at": stamp(p.ReachedAt),
	})
}

func milestoneTitle(p events.AnalyticsMilestoneReachedV1) string {
	switch p.Milestone {
	case "first_order":
		return "your first order"
	case "order_count":
		return strconv.FormatInt(p.Threshold, 10) + " orders"
	}
	if p.Currency != nil {
		return money.New(p.Threshold, *p.Currency).String() + " in sales"
	}
	return strconv.FormatInt(p.Threshold, 10) + " in sales"
}

func (s *Service) invitedSystemAdminEmails(ctx context.Context) ([]string, error) {
	var emails []string
	err := s.inAnyTenant(ctx, eventbus.PlatformTenantID, func(tx pgx.Tx, _ string) error {
		var err error
		emails, err = s.repo.SystemAdminEmails(ctx, tx)
		return err
	})
	return emails, err
}

// shopAdmins resolves each of the shop's admins' channel through
// yc-preference; admins without a usable address are skipped.
func (s *Service) shopAdmins(ctx context.Context, tenantID string) ([]domain.Recipient, error) {
	var ids []string
	err := s.inTenant(ctx, tenantID, func(tx pgx.Tx, tenantID string) error {
		var err error
		ids, err = s.repo.ShopAdmins(ctx, tx, tenantID)
		return err
	})
	if err != nil {
		return nil, err
	}
	ctx = serviceContext(ctx, tenantID)
	var out []domain.Recipient
	for _, id := range ids {
		r, ok, err := s.customer(ctx, id)
		if err != nil {
			return nil, err
		}
		if !ok {
			_ = s.skipped(ctx, nil, domain.TypeAdminAlert, id)
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// notifyAll queues one message per recipient, once per event.
func (s *Service) notifyAll(ctx context.Context, tenantID, eventID string, t domain.Type, to []domain.Recipient, msgCtx map[string]any) error {
	if len(to) == 0 {
		return s.skipped(ctx, nil, t, eventID)
	}
	return s.inAnyTenant(ctx, tenantID, func(tx pgx.Tx, tenantID string) error {
		for _, r := range to {
			who := r.UserID
			if who == "" {
				who = r.Address
			}
			if _, err := s.queue(ctx, tx, tenantID, request{
				Type: t, Reference: "event:" + eventID + ":" + string(t) + ":" + who, To: r, Context: msgCtx,
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

// inAnyTenant is inTenant that also accepts the platform tenant, for
// messages to system admins. Every query filters on tenant_id, so the
// platform context's view of all shops does not leak into them.
func (s *Service) inAnyTenant(ctx context.Context, tenantID string, fn func(tx pgx.Tx, tenantID string) error) error {
	if tenantID != eventbus.PlatformTenantID {
		return s.inTenant(ctx, tenantID, fn)
	}
	return dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error { return fn(tx, tenantID) })
}

func number(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
}

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") }
