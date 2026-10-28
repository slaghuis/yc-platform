package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/domain"
)

// Orders reports a user's unfinished orders (yc-orders).
type Orders interface {
	// OpenOrders counts the user's orders and returns that are not yet
	// finished. The tenant comes from ctx.
	OpenOrders(ctx context.Context, userID string) (int, error)
	// AddressInUse reports whether addressID is on any Status.Open() order
	// (issue #32). The tenant comes from ctx.
	AddressInUse(ctx context.Context, addressID string) (bool, error)
}

// Who asked for a deletion (identity.user.deleted.v1 requested_by).
const (
	requestedByUser        = "user"
	requestedBySystemAdmin = "system_admin"
)

// DeleteMe deletes the calling customer's account (ADR-005).
func (s *Service) DeleteMe(ctx context.Context) error {
	a, _ := authctx.FromContext(ctx)
	if !a.Authenticated() {
		return ycerrors.Unauthorized("Authentication required")
	}
	if a.ActorType != authctx.ActorCustomer {
		return ycerrors.Forbidden("Only customers can delete their own account")
	}
	return s.deleteUser(ctx, a.TenantID, a.ActorID, requestedByUser)
}

// DeleteUser deletes user id on a system admin's request (ADR-005). System
// admins and visitors cannot be deleted.
func (s *Service) DeleteUser(ctx context.Context, id string) error {
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorSystemAdmin {
		return ycerrors.Forbidden("Only system admins can delete users")
	}
	// RLS lets a _platform transaction read every tenant but write none.
	found, err := s.getUser(ctx, dbkit.PlatformTenantID, id)
	if err != nil {
		return err
	}
	switch found.Role {
	case domain.RoleSystemAdmin:
		return ycerrors.Conflict("System admins cannot be deleted")
	case domain.RoleVisitor, domain.RoleService:
		return ycerrors.Conflict("Only customer and shop admin accounts can be deleted")
	case domain.RoleCustomer, domain.RoleShopAdmin:
	}
	return s.deleteUser(ctx, found.TenantID, id, requestedBySystemAdmin)
}

// deleteUser refuses while the user has unfinished orders, then anonymises
// the user, revokes its sessions, records its pseudonym and publishes
// identity.user.deleted.v1 and yc-identity's own
// platform.gdpr.cascade_completed.v1 in one transaction.
func (s *Service) deleteUser(ctx context.Context, tenantID, id, requestedBy string) error {
	a, _ := authctx.FromContext(ctx)
	if s.cfg.Orders != nil {
		inTenant := authctx.WithAuth(ctx, authctx.AuthContext{
			TenantID: tenantID, ActorID: a.ActorID, ActorType: a.ActorType, CorrelationID: a.CorrelationID,
		})
		open, err := s.cfg.Orders.OpenOrders(inTenant, id)
		if err != nil {
			return ycerrors.Unavailable("The account cannot be deleted right now; try again later").Wrap(err)
		}
		if open > 0 {
			noun := "orders or returns"
			if open == 1 {
				noun = "order or return"
			}
			return ycerrors.Conflict(fmt.Sprintf(
				"The account has %d unfinished %s; delete it once they are complete", open, noun))
		}
	}

	now := s.now().UTC()
	pseudonym := uuid.NewString()
	err := dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		user, err := s.users.GetForUpdate(ctx, tx, id)
		if err != nil {
			return err
		}
		if user.Status == domain.StatusDeleted {
			return ycerrors.NotFound("User", id)
		}
		if err := s.pseudonyms.Insert(ctx, tx, tenantID, id, pseudonym, requestedBy, now); err != nil {
			return err
		}
		if err := s.users.Anonymise(ctx, tx, id, now); err != nil {
			return err
		}
		if err := s.addresses.DeleteAll(ctx, tx, id); err != nil {
			return err
		}
		if err := s.billing.Delete(ctx, tx, id); err != nil {
			return err
		}
		links, err := s.oauth.UnlinkAll(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := s.resets.DeleteUser(ctx, tx, id); err != nil {
			return err
		}
		if err := s.sessions.RevokeUser(ctx, tx, id); err != nil {
			return err
		}

		deleted := events.IdentityUserDeletedV1{
			UserID:      id,
			Pseudonym:   pseudonym,
			Role:        string(user.Role),
			RequestedBy: requestedBy,
			DeletedAt:   now,
		}
		err = s.bus.Publish(ctx, tx, eventbus.Event{
			EventType: events.TypeIdentityUserDeletedV1,
			TenantID:  tenantID,
			Actor:     eventbus.Actor{Type: string(a.ActorType), ID: a.ActorID},
			Payload:   deleted,
		})
		if err != nil {
			return err
		}
		return s.bus.Publish(ctx, tx,
			events.GDPRCompleted(tenantID, serviceName, deleted, events.GDPRAnonymized, 1+links, now))
	})
	if err != nil {
		return fmt.Errorf("delete user %s: %w", id, err)
	}
	return nil
}
