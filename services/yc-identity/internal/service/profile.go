package service

import (
	"context"

	"github.com/jackc/pgx/v5"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/domain"
)

// PatchMe applies a partial profile update to the caller's own user (#37).
func (s *Service) PatchMe(ctx context.Context, patch domain.ProfilePatch) (domain.User, error) {
	a, err := requireProfileActor(ctx)
	if err != nil {
		return domain.User{}, err
	}
	var out domain.User
	err = s.inTenant(ctx, a.TenantID, func(tx pgx.Tx) error {
		user, err := s.users.GetForUpdate(ctx, tx, a.ActorID)
		if err != nil {
			return err
		}
		if user.Status != domain.StatusActive {
			return ycerrors.Forbidden("Account is not active")
		}
		patch.Apply(&user)
		out, err = s.users.UpdateProfile(ctx, tx, user)
		return err
	})
	return out, err
}

// requireProfileActor allows customer, shop_admin and system_admin to edit
// their own /me profile. Visitors are refused.
func requireProfileActor(ctx context.Context) (authctx.AuthContext, error) {
	a, _ := authctx.FromContext(ctx)
	if !a.Authenticated() {
		return a, ycerrors.Unauthorized("Authentication required")
	}
	switch a.ActorType {
	case authctx.ActorCustomer, authctx.ActorShopAdmin:
		if err := validateShopTenant(a.TenantID); err != nil {
			return a, err
		}
	case authctx.ActorSystemAdmin:
		// Own account on the platform (or any) tenant.
	case authctx.ActorVisitor, authctx.ActorService:
		return a, ycerrors.Forbidden("Visitors cannot update a profile").
			WithInternal("actor_type %s", a.ActorType)
	default:
		return a, ycerrors.Forbidden("Visitors cannot update a profile").
			WithInternal("actor_type %s", a.ActorType)
	}
	return a, nil
}
