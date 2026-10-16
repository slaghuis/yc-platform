package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/domain"
)

// Suspend sets user id to suspended, revokes refresh families, and publishes
// identity.user.suspended.v1 (ADR-030 rule 8). System admins only; cannot
// suspend self or the last active system admin.
func (s *Service) Suspend(ctx context.Context, id string, reason string) (domain.User, error) {
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorSystemAdmin {
		return domain.User{}, ycerrors.Forbidden("Only system admins can suspend users")
	}
	if id == a.ActorID {
		return domain.User{}, ycerrors.Conflict("You cannot suspend yourself")
	}

	found, err := s.getUser(ctx, dbkit.PlatformTenantID, id)
	if err != nil {
		return domain.User{}, err
	}
	if found.Status == domain.StatusDeleted {
		return domain.User{}, ycerrors.NotFound("User", id)
	}
	if found.Status == domain.StatusSuspended {
		return found, nil
	}
	if found.Status != domain.StatusActive {
		return domain.User{}, ycerrors.Conflict(fmt.Sprintf("User is %s", found.Status))
	}

	var out domain.User
	err = s.inTenant(ctx, found.TenantID, func(tx pgx.Tx) error {
		user, err := s.users.GetForUpdate(ctx, tx, id)
		if err != nil {
			return err
		}
		if user.Status == domain.StatusSuspended {
			out = user
			return nil
		}
		if user.Status != domain.StatusActive {
			return ycerrors.Conflict(fmt.Sprintf("User is %s", user.Status))
		}
		if user.Role == domain.RoleSystemAdmin {
			n, err := s.users.CountActive(ctx, tx, dbkit.PlatformTenantID, domain.RoleSystemAdmin)
			if err != nil {
				return err
			}
			if n <= 1 {
				return ycerrors.Conflict("The last active system admin cannot be suspended")
			}
		}
		if out, err = s.users.SetStatus(ctx, tx, id, domain.StatusSuspended); err != nil {
			return err
		}
		if err := s.sessions.RevokeUser(ctx, tx, id); err != nil {
			return err
		}
		var reasonPtr *string
		if reason = strings.TrimSpace(reason); reason != "" {
			reasonPtr = &reason
		}
		return s.bus.Publish(ctx, tx, eventbus.Event{
			EventType: events.TypeIdentityUserSuspendedV1,
			TenantID:  user.TenantID,
			Actor:     callerActor(a),
			Payload: events.IdentityUserSuspendedV1{
				UserID: user.ID, TenantID: user.TenantID, Role: string(user.Role),
				Reason: reasonPtr, SuspendedAt: s.now().UTC(),
			},
		})
	})
	if err != nil {
		return domain.User{}, fmt.Errorf("suspend user %s: %w", id, err)
	}
	return out, nil
}

// Reactivate restores a suspended user (ADR-030 rule 8).
func (s *Service) Reactivate(ctx context.Context, id string) (domain.User, error) {
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorSystemAdmin {
		return domain.User{}, ycerrors.Forbidden("Only system admins can reactivate users")
	}

	found, err := s.getUser(ctx, dbkit.PlatformTenantID, id)
	if err != nil {
		return domain.User{}, err
	}
	if found.Status == domain.StatusDeleted {
		return domain.User{}, ycerrors.NotFound("User", id)
	}
	if found.Status == domain.StatusActive {
		return found, nil
	}
	if found.Status != domain.StatusSuspended {
		return domain.User{}, ycerrors.Conflict(fmt.Sprintf("User is %s", found.Status))
	}

	var out domain.User
	err = s.inTenant(ctx, found.TenantID, func(tx pgx.Tx) error {
		user, err := s.users.GetForUpdate(ctx, tx, id)
		if err != nil {
			return err
		}
		if user.Status == domain.StatusActive {
			out = user
			return nil
		}
		if user.Status != domain.StatusSuspended {
			return ycerrors.Conflict(fmt.Sprintf("User is %s", user.Status))
		}
		if out, err = s.users.SetStatus(ctx, tx, id, domain.StatusActive); err != nil {
			return err
		}
		return s.bus.Publish(ctx, tx, eventbus.Event{
			EventType: events.TypeIdentityUserReactivatedV1,
			TenantID:  user.TenantID,
			Actor:     callerActor(a),
			Payload: events.IdentityUserReactivatedV1{
				UserID: user.ID, TenantID: user.TenantID, Role: string(user.Role),
				ReactivatedAt: s.now().UTC(),
			},
		})
	})
	if err != nil {
		return domain.User{}, fmt.Errorf("reactivate user %s: %w", id, err)
	}
	return out, nil
}

// Demote lowers a shop_admin to customer in the same tenant (ADR-030 rule 9).
// System admins are not demoted; they are suspended.
func (s *Service) Demote(ctx context.Context, id string) (domain.User, error) {
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorSystemAdmin {
		return domain.User{}, ycerrors.Forbidden("Only system admins can demote users")
	}

	found, err := s.getUser(ctx, dbkit.PlatformTenantID, id)
	if err != nil {
		return domain.User{}, err
	}
	if found.Role == domain.RoleSystemAdmin {
		return domain.User{}, ycerrors.Conflict("System admins cannot be demoted; suspend them instead")
	}
	if found.Role != domain.RoleShopAdmin {
		return domain.User{}, ycerrors.Conflict("Only shop_admin users can be demoted to customer")
	}
	if found.Status != domain.StatusActive && found.Status != domain.StatusSuspended {
		return domain.User{}, ycerrors.Conflict(fmt.Sprintf("User is %s", found.Status))
	}

	var out domain.User
	err = s.inTenant(ctx, found.TenantID, func(tx pgx.Tx) error {
		user, err := s.users.GetForUpdate(ctx, tx, id)
		if err != nil {
			return err
		}
		if user.Role != domain.RoleShopAdmin {
			return ycerrors.Conflict("Only shop_admin users can be demoted to customer")
		}
		if out, err = s.users.SetRole(ctx, tx, id, domain.RoleCustomer); err != nil {
			return err
		}
		return s.bus.Publish(ctx, tx, eventbus.Event{
			EventType: events.TypeIdentityUserDemotedV1,
			TenantID:  user.TenantID,
			Actor:     callerActor(a),
			Payload: events.IdentityUserDemotedV1{
				UserID:    user.ID,
				FromRole:  events.IdentityUserDemotedV1FromRoleShopAdmin,
				ToRole:    events.IdentityUserDemotedV1ToRoleCustomer,
				DemotedAt: s.now().UTC(),
			},
		})
	})
	if err != nil {
		return domain.User{}, fmt.Errorf("demote user %s: %w", id, err)
	}
	return out, nil
}
