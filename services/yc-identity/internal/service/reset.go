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
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/domain"
)

// RequestPasswordReset stores a reset token for the active user with email
// in the tenant yc-gateway resolved, publishes
// identity.user.password_reset_requested.v1, which carries no token, and
// then has yc-message email the reset link for host. An unknown email does
// nothing and is not an error, so the endpoint cannot be used to discover
// registered emails; for the same reason a failed send is only logged.
func (s *Service) RequestPasswordReset(ctx context.Context, email, host string) error {
	a, _ := authctx.FromContext(ctx)
	if err := validateTenant(a.TenantID); err != nil {
		return err
	}
	norm, ok := domain.NormaliseEmail(email)
	if !ok {
		return ycerrors.Validation().Field("email", "invalid", "Email is not valid").Build()
	}
	var (
		user    domain.User
		raw, id string
		expires time.Time
	)
	err := s.inTenant(ctx, a.TenantID, func(tx pgx.Tx) error {
		var err error
		user, err = s.users.GetByEmail(ctx, tx, a.TenantID, norm)
		if ycerrors.HasCode(err, ycerrors.CodeNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if user.Status != domain.StatusActive {
			return nil
		}
		var hash []byte
		raw, hash = newSecret()
		id = uuid.NewString()
		now := s.now()
		expires = now.Add(s.cfg.ResetTokenTTL)
		if err := s.resets.Insert(ctx, tx, id, user.TenantID, user.ID, hash, expires); err != nil {
			return err
		}
		err = s.bus.Publish(ctx, tx, eventbus.Event{
			EventType: events.TypeIdentityUserPasswordResetRequestedV1,
			TenantID:  user.TenantID,
			Actor:     callerActor(a),
			Payload: events.IdentityUserPasswordResetRequestedV1{
				UserID:      user.ID,
				Email:       user.Email,
				RequestID:   id,
				ExpiresAt:   expires.UTC(),
				RequestedAt: now.UTC(),
			},
		})
		if err != nil {
			return err
		}
		if s.cfg.LogResetLinks {
			telemetry.LoggerFromContext(ctx).Info("password reset token (log_reset_links is for development only)",
				"request_id", id, "user_id", user.ID, "reset_token", raw)
		}
		return nil
	})
	if err != nil || raw == "" {
		return err
	}
	s.sendResetLink(ctx, user, id, raw, host, expires)
	return nil
}

// sendResetLink has yc-message email the reset link, once per request ID.
func (s *Service) sendResetLink(ctx context.Context, user domain.User, requestID, raw, host string, expires time.Time) {
	if s.cfg.Messages == nil {
		return
	}
	log := telemetry.LoggerFromContext(ctx)
	link := strings.ReplaceAll(s.cfg.ResetURL, "{token}", url.QueryEscape(raw))
	if strings.Contains(link, "{host}") {
		if host == "" {
			log.Warn("password reset link not sent: the request has no host", "request_id", requestID)
			return
		}
		link = strings.ReplaceAll(link, "{host}", host)
	}
	err := s.cfg.Messages.SendPasswordReset(ctx, "password-reset:"+requestID, user.ID, user.Email, link,
		expires.UTC().Format("2006-01-02 15:04 UTC"))
	if err != nil {
		log.Error("password reset link not sent", "request_id", requestID, "user_id", user.ID, "error", err)
	}
}

// ConfirmPasswordReset sets a new password with a reset token, revokes the
// user's refresh tokens and outstanding reset tokens, and publishes
// identity.user.password_changed.v1.
func (s *Service) ConfirmPasswordReset(ctx context.Context, token, pw string) error {
	a, _ := authctx.FromContext(ctx)
	if err := validateTenant(a.TenantID); err != nil {
		return err
	}
	if err := domain.ValidatePassword(pw); err != nil {
		return err
	}
	hash, err := s.hasher.Hash(pw)
	if err != nil {
		return err
	}
	invalid := ycerrors.Validation().Field("token", "invalid", "Reset link is invalid or has expired").Build()
	err = s.inTenant(ctx, a.TenantID, func(tx pgx.Tx) error {
		now := s.now()
		userID, err := s.resets.Use(ctx, tx, a.TenantID, hashSecret(token), now)
		if ycerrors.HasCode(err, ycerrors.CodeNotFound) {
			return invalid
		}
		if err != nil {
			return err
		}
		user, err := s.users.GetForUpdate(ctx, tx, userID)
		if err != nil {
			return err
		}
		if user.Status != domain.StatusActive || user.Email == "" {
			return invalid
		}
		if err := s.users.SetPassword(ctx, tx, user.ID, hash); err != nil {
			return err
		}
		if err := s.resets.UseAll(ctx, tx, user.ID, now); err != nil {
			return err
		}
		if err := s.sessions.RevokeUser(ctx, tx, user.ID); err != nil {
			return err
		}
		return s.bus.Publish(ctx, tx, eventbus.Event{
			EventType: events.TypeIdentityUserPasswordChangedV1,
			TenantID:  user.TenantID,
			Actor:     eventbus.Actor{Type: string(user.Role.ActorType()), ID: user.ID},
			Payload: events.IdentityUserPasswordChangedV1{
				UserID:    user.ID,
				Method:    events.IdentityUserPasswordChangedV1MethodReset,
				ChangedAt: now.UTC(),
			},
		})
	})
	if err != nil {
		return fmt.Errorf("confirm password reset: %w", err)
	}
	return nil
}
