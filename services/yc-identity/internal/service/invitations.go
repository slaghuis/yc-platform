package service

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/repository"
)

// CreateInvitation invites email to role in tenant (ADR-030 rules 1–5).
// Only system admins may invite. acceptHost is the hostname for the
// accept link; when empty, platform_hosts[0] or the shop_hosts projection
// is used.
func (s *Service) CreateInvitation(ctx context.Context, in domain.InviteInput) (domain.Invitation, error) {
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorSystemAdmin {
		return domain.Invitation{}, ycerrors.Forbidden("Only system admins can invite")
	}
	return s.createInvitation(ctx, in, a.ActorID, callerActor(a))
}

// createInvitation is shared by the API and the tenant-provisioned consumer.
func (s *Service) createInvitation(ctx context.Context, in domain.InviteInput, invitedBy string, actor eventbus.Actor) (domain.Invitation, error) {
	if invitedBy == "" {
		return domain.Invitation{}, ycerrors.Validation().Message("Invitation actor is required").Build()
	}
	host, err := s.resolveAcceptHost(ctx, in.TenantID, in.AcceptHost)
	if err != nil {
		return domain.Invitation{}, err
	}

	raw, hash := newSecret()
	now := s.now()
	expires := now.Add(s.cfg.InviteTTL)
	inv := domain.Invitation{
		ID: uuid.NewString(), TenantID: in.TenantID, Email: in.Email, Role: in.Role,
		DisplayName: in.DisplayName, ExpiresAt: expires, InvitedBy: invitedBy,
		CreatedAt: now, UpdatedAt: now,
	}

	err = s.inTenant(ctx, in.TenantID, func(tx pgx.Tx) error {
		existing, err := s.users.GetByEmail(ctx, tx, in.TenantID, in.Email)
		if err == nil {
			switch existing.Role {
			case domain.RoleShopAdmin, domain.RoleSystemAdmin:
				return ycerrors.Conflict("This email is already an admin of this tenant")
			case domain.RoleCustomer, domain.RoleVisitor:
				// Customer upgrade path; visitors with an email are rare.
			case domain.RoleService:
				return ycerrors.Conflict("This email cannot be invited")
			default:
				return ycerrors.Conflict("This email cannot be invited")
			}
			if existing.Status == domain.StatusDeleted {
				return ycerrors.Conflict("This email belongs to a deleted account")
			}
		} else if !ycerrors.HasCode(err, ycerrors.CodeNotFound) {
			return err
		}
		if inv, err = s.invitations.Insert(ctx, tx, inv, hash); err != nil {
			return err
		}
		return s.publishInvited(ctx, tx, inv, actor, now)
	})
	if err != nil {
		return domain.Invitation{}, fmt.Errorf("create invitation: %w", err)
	}
	s.sendInviteLink(ctx, inv, raw, host, expires)
	return inv, nil
}

// ListInvitations returns a page of invitations, newest first.
func (s *Service) ListInvitations(ctx context.Context, f repository.InvitationFilter, after *repository.InvitationCursor, limit int) ([]domain.Invitation, *repository.InvitationCursor, error) {
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorSystemAdmin {
		return nil, nil, ycerrors.Forbidden("Only system admins can list invitations")
	}
	if f.TenantID != "" {
		if err := eventbus.ValidateTenantID(f.TenantID); err != nil {
			return nil, nil, ycerrors.Validation().Field("tenant_id", "invalid", "Tenant ID is not valid").Wrap(err)
		}
	}
	if f.Status != "" && !f.Status.Valid() {
		return nil, nil, ycerrors.Validation().Field("status", "invalid", "Unknown invitation status").Build()
	}
	limit = min(max(limit, 1), MaxPageSize)
	now := s.now()
	var invs []domain.Invitation
	err := s.inTenant(ctx, dbkit.PlatformTenantID, func(tx pgx.Tx) error {
		var err error
		invs, err = s.invitations.List(ctx, tx, f, after, limit+1, now)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	var next *repository.InvitationCursor
	if len(invs) > limit {
		invs = invs[:limit]
		last := invs[limit-1]
		next = &repository.InvitationCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return invs, next, nil
}

// ResendInvitation issues a new token for a pending invitation and emails it.
func (s *Service) ResendInvitation(ctx context.Context, id string) (domain.Invitation, error) {
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorSystemAdmin {
		return domain.Invitation{}, ycerrors.Forbidden("Only system admins can resend invitations")
	}
	found, err := s.getInvitation(ctx, id)
	if err != nil {
		return domain.Invitation{}, err
	}
	if found.Status != domain.InvitationPending {
		return domain.Invitation{}, ycerrors.Conflict("Only pending invitations can be resent")
	}
	host, err := s.resolveAcceptHost(ctx, found.TenantID, "")
	if err != nil {
		return domain.Invitation{}, err
	}

	raw, hash := newSecret()
	now := s.now()
	expires := now.Add(s.cfg.InviteTTL)
	var inv domain.Invitation
	err = dbkit.WithTenantTx(ctx, s.db, found.TenantID, func(tx pgx.Tx) error {
		var err error
		if inv, err = s.invitations.Resend(ctx, tx, id, hash, expires, now); err != nil {
			return err
		}
		return s.publishInvited(ctx, tx, inv, callerActor(a), now)
	})
	if err != nil {
		return domain.Invitation{}, fmt.Errorf("resend invitation %s: %w", id, err)
	}
	s.sendInviteLink(ctx, inv, raw, host, expires)
	return inv, nil
}

// RevokeInvitation voids a pending invitation (ADR-030).
func (s *Service) RevokeInvitation(ctx context.Context, id string) error {
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorSystemAdmin {
		return ycerrors.Forbidden("Only system admins can revoke invitations")
	}
	found, err := s.getInvitation(ctx, id)
	if err != nil {
		return err
	}
	now := s.now()
	err = dbkit.WithTenantTx(ctx, s.db, found.TenantID, func(tx pgx.Tx) error {
		inv, err := s.invitations.Revoke(ctx, tx, id, now)
		if err != nil {
			return err
		}
		return s.bus.Publish(ctx, tx, eventbus.Event{
			EventType: events.TypeIdentityUserInvitationRevokedV1,
			TenantID:  inv.TenantID,
			Actor:     callerActor(a),
			Payload: events.IdentityUserInvitationRevokedV1{
				InvitationID: inv.ID,
				TenantID:     inv.TenantID,
				Email:        inv.Email,
				Role:         string(inv.Role),
				RevokedAt:    now.UTC(),
			},
		})
	})
	if err != nil {
		return fmt.Errorf("revoke invitation %s: %w", id, err)
	}
	return nil
}

// AcceptInvitation activates the invited role with password (ADR-030 rules
// 5–6). Unknown, used and expired tokens share one error. Failures are
// throttled like login. An existing customer of the shop keeps their user
// ID and must supply their current password.
func (s *Service) AcceptInvitation(ctx context.Context, in domain.AcceptInviteInput) (domain.Session, error) {
	a, _ := authctx.FromContext(ctx)
	if err := validateTenant(a.TenantID); err != nil {
		return domain.Session{}, err
	}
	invalid := errBadInvite("unknown, used or expired token")
	tokenKey := failureKey("invite:" + in.Token)
	if err := s.checkLoginFailures(ctx, a.TenantID, tokenKey[:]); err != nil {
		return domain.Session{}, err
	}
	if in.Token == "" {
		s.hasher.VerifyNothing(in.Password)
		return domain.Session{}, s.loginFailed(ctx, a.TenantID, tokenKey[:], invalid)
	}

	var sess domain.Session
	err := s.inTenant(ctx, a.TenantID, func(tx pgx.Tx) error {
		now := s.now()
		recordFail := func() error {
			if s.cfg.LoginMaxFailures <= 0 {
				return nil
			}
			return s.failures.Record(ctx, tx, a.TenantID, tokenKey[:], now, now.Add(-s.cfg.LoginFailureWindow))
		}
		inv, err := s.invitations.GetByTokenHash(ctx, tx, a.TenantID, hashSecret(in.Token), now)
		if ycerrors.HasCode(err, ycerrors.CodeNotFound) {
			s.hasher.VerifyNothing(in.Password)
			_ = recordFail()
			return invalid
		}
		if err != nil {
			return err
		}

		existing, err := s.users.GetByEmail(ctx, tx, inv.TenantID, inv.Email)
		switch {
		case ycerrors.HasCode(err, ycerrors.CodeNotFound):
			hash, err := s.hasher.Hash(in.Password)
			if err != nil {
				return err
			}
			display := in.DisplayName
			if display == "" {
				display = inv.DisplayName
			}
			user, err := s.users.Create(ctx, tx, inv.TenantID, inv.Role, domain.Registration{
				Email: inv.Email, Password: in.Password, DisplayName: display,
			}, hash)
			if err != nil {
				return err
			}
			if err := s.invitations.Accept(ctx, tx, inv.ID, user.ID, now); err != nil {
				return err
			}
			if sess, _, err = s.issueSession(ctx, tx, user, ""); err != nil {
				return err
			}
			return s.publishAccepted(ctx, tx, inv, user, false, now)
		case err != nil:
			return err
		}

		switch existing.Role {
		case domain.RoleShopAdmin, domain.RoleSystemAdmin:
			return ycerrors.Conflict("This email is already an admin of this tenant")
		case domain.RoleVisitor, domain.RoleService:
			s.hasher.VerifyNothing(in.Password)
			_ = recordFail()
			return invalid
		case domain.RoleCustomer:
			if existing.PasswordHash == "" {
				s.hasher.VerifyNothing(in.Password)
				_ = recordFail()
				return invalid
			}
			match, err := s.hasher.Verify(in.Password, existing.PasswordHash)
			if err != nil {
				return fmt.Errorf("accept invitation: verify: %w", err)
			}
			if !match {
				_ = recordFail()
				return invalid
			}
			if existing.Status != domain.StatusActive && existing.Status != domain.StatusSuspended {
				_ = recordFail()
				return invalid
			}
			display := in.DisplayName
			user, err := s.users.ActivateAdmin(ctx, tx, existing.ID, inv.Role, display)
			if err != nil {
				return err
			}
			if err := s.invitations.Accept(ctx, tx, inv.ID, user.ID, now); err != nil {
				return err
			}
			if err := s.sessions.RevokeUser(ctx, tx, user.ID); err != nil {
				return err
			}
			if sess, _, err = s.issueSession(ctx, tx, user, ""); err != nil {
				return err
			}
			return s.publishAccepted(ctx, tx, inv, user, true, now)
		default:
			s.hasher.VerifyNothing(in.Password)
			_ = recordFail()
			return invalid
		}
	})
	if err != nil {
		return domain.Session{}, err
	}
	return sess, nil
}

// OnTenantProvisioned records the shop hostname and, when
// primary_admin_email is set, creates a shop_admin invitation as the
// envelope actor (ADR-030 rule 7).
func (s *Service) OnTenantProvisioned(ctx context.Context, p events.PlatformTenantProvisionedV1, actor eventbus.Actor) error {
	if err := validateShopTenant(p.TenantID); err != nil {
		return err
	}
	primary := ""
	if len(p.Hostnames) > 0 {
		primary = strings.ToLower(strings.TrimSpace(p.Hostnames[0]))
	}
	if primary != "" {
		err := dbkit.WithTenantTx(ctx, s.db, p.TenantID, func(tx pgx.Tx) error {
			return s.shopHosts.Put(ctx, tx, p.TenantID, primary, p.ProvisionedAt)
		})
		if err != nil {
			return err
		}
	}
	if p.PrimaryAdminEmail == nil || strings.TrimSpace(*p.PrimaryAdminEmail) == "" {
		return nil
	}
	in, err := domain.NewInviteInput(p.TenantID, *p.PrimaryAdminEmail, string(domain.RoleShopAdmin), "", primary)
	if err != nil {
		return err
	}
	invitedBy := actor.ID
	if invitedBy == "" {
		return ycerrors.Validation().Message("Provisioning event has no actor").Build()
	}
	_, err = s.createInvitation(ctx, in, invitedBy, actor)
	if ycerrors.HasCode(err, ycerrors.CodeConflict) {
		// Already invited or already an admin: nothing more to do.
		telemetry.LoggerFromContext(ctx).Info("primary admin invite skipped",
			"tenant_id", p.TenantID, "error", err.Error())
		return nil
	}
	return err
}

// OnTenantUpdated refreshes the shop's primary hostname.
func (s *Service) OnTenantUpdated(ctx context.Context, p events.PlatformTenantUpdatedV1) error {
	if err := validateShopTenant(p.TenantID); err != nil {
		return err
	}
	primary := strings.ToLower(strings.TrimSpace(p.PrimaryHostname))
	if primary == "" && len(p.Hostnames) > 0 {
		primary = strings.ToLower(strings.TrimSpace(p.Hostnames[0]))
	}
	if primary == "" {
		return nil
	}
	return dbkit.WithTenantTx(ctx, s.db, p.TenantID, func(tx pgx.Tx) error {
		return s.shopHosts.Put(ctx, tx, p.TenantID, primary, p.UpdatedAt)
	})
}

// OnTenantDeleted clears the shop hostname projection.
func (s *Service) OnTenantDeleted(ctx context.Context, p events.PlatformTenantDeletedV1) error {
	if err := validateShopTenant(p.TenantID); err != nil {
		return err
	}
	return dbkit.WithTenantTx(ctx, s.db, p.TenantID, func(tx pgx.Tx) error {
		return s.shopHosts.Delete(ctx, tx, p.TenantID)
	})
}

func (s *Service) getInvitation(ctx context.Context, id string) (domain.Invitation, error) {
	var inv domain.Invitation
	err := s.inTenant(ctx, dbkit.PlatformTenantID, func(tx pgx.Tx) error {
		var err error
		inv, err = s.invitations.Get(ctx, tx, id)
		return err
	})
	return inv, err
}

func (s *Service) resolveAcceptHost(ctx context.Context, tenantID, acceptHost string) (string, error) {
	if acceptHost != "" {
		return acceptHost, nil
	}
	if tenantID == dbkit.PlatformTenantID {
		if len(s.cfg.PlatformHosts) == 0 {
			return "", ycerrors.Validation().Field("accept_host", "required",
				"accept_host is required when no platform_hosts are configured").Build()
		}
		return s.cfg.PlatformHosts[0], nil
	}
	var host string
	err := s.inTenant(ctx, tenantID, func(tx pgx.Tx) error {
		h, ok, err := s.shopHosts.Primary(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		if ok {
			host = h
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if host == "" {
		return "", ycerrors.Validation().Field("accept_host", "required",
			"accept_host is required when the shop hostname is not known yet").Build()
	}
	return host, nil
}

func (s *Service) publishInvited(ctx context.Context, tx pgx.Tx, inv domain.Invitation, actor eventbus.Actor, now time.Time) error {
	var displayName *string
	if inv.DisplayName != "" {
		displayName = &inv.DisplayName
	}
	return s.bus.Publish(ctx, tx, eventbus.Event{
		EventType: events.TypeIdentityUserInvitedV1,
		TenantID:  inv.TenantID,
		Actor:     actor,
		Payload: events.IdentityUserInvitedV1{
			InvitationID: inv.ID,
			TenantID:     inv.TenantID,
			Email:        inv.Email,
			Role:         string(inv.Role),
			DisplayName:  displayName,
			ExpiresAt:    inv.ExpiresAt.UTC(),
			InvitedAt:    now.UTC(),
		},
	})
}

func (s *Service) publishAccepted(ctx context.Context, tx pgx.Tx, inv domain.Invitation, user domain.User, upgraded bool, now time.Time) error {
	return s.bus.Publish(ctx, tx, eventbus.Event{
		EventType: events.TypeIdentityUserInvitationAcceptedV1,
		TenantID:  user.TenantID,
		Actor:     eventbus.Actor{Type: string(user.Role.ActorType()), ID: user.ID},
		Payload: events.IdentityUserInvitationAcceptedV1{
			InvitationID:     inv.ID,
			UserID:           user.ID,
			TenantID:         user.TenantID,
			Email:            inv.Email,
			Role:             string(user.Role),
			UpgradedCustomer: upgraded,
			AcceptedAt:       now.UTC(),
		},
	})
}

func errBadInvite(internal string, args ...any) error {
	return ycerrors.Validation().Field("token", "invalid", "Invitation link is invalid or has expired").Build().
		WithInternal(internal, args...)
}

// sendInviteLink has yc-message email the accept link, once per invitation
// token cycle. The call runs in the invitation's tenant so shop invites
// leave _platform (ADR-030 rule 4).
func (s *Service) sendInviteLink(ctx context.Context, inv domain.Invitation, raw, host string, expires time.Time) {
	if s.cfg.Messages == nil {
		return
	}
	log := telemetry.LoggerFromContext(ctx)
	link := strings.ReplaceAll(s.cfg.AcceptURL, "{token}", url.QueryEscape(raw))
	if strings.Contains(link, "{host}") {
		if host == "" {
			log.Warn("admin invite not sent: no accept host", "invitation_id", inv.ID)
			return
		}
		link = strings.ReplaceAll(link, "{host}", host)
	}
	a, _ := authctx.FromContext(ctx)
	sendCtx := authctx.WithAuth(ctx, authctx.AuthContext{
		TenantID: inv.TenantID, ActorID: a.ActorID, ActorType: a.ActorType, CorrelationID: a.CorrelationID,
	})
	err := s.cfg.Messages.SendAdminInvite(sendCtx,
		"admin-invite:"+inv.ID+":"+expires.UTC().Format(time.RFC3339Nano),
		inv.Email, link, expires.UTC().Format("2006-01-02 15:04 UTC"), string(inv.Role))
	if err != nil {
		log.Error("admin invite not sent", "invitation_id", inv.ID, "error", err)
	}
	if s.cfg.LogInviteLinks {
		log.Info("admin invite token (log_invite_links is for development only)",
			"invitation_id", inv.ID, "invite_token", raw)
	}
}
