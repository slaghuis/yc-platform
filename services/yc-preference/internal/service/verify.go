package service

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	"github.com/slaghuis/yc-platform/services/yc-preference/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-preference/internal/integration"
)

// Messages is yc-message's verification API.
type Messages interface {
	StartVerification(ctx context.Context, userID, channel, address string) (integration.Verification, error)
	ConfirmVerification(ctx context.Context, userID, channel, address, code string) (integration.Verification, error)
}

// ForUser returns a user's stored preferences for another service, such as
// yc-message choosing where to send. NotFound when the user has none.
func (s *Service) ForUser(ctx context.Context, userID string) (domain.Preferences, error) {
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorService {
		return domain.Preferences{}, ycerrors.Forbidden("Services only").WithInternal("actor_type %s", a.ActorType)
	}
	if err := validateShopTenant(a.TenantID); err != nil {
		return domain.Preferences{}, err
	}
	var p domain.Preferences
	err := dbkit.WithTenantTx(ctx, s.db, a.TenantID, func(tx pgx.Tx) error {
		var (
			ok  bool
			err error
		)
		p, ok, err = s.repo.Get(ctx, tx, a.TenantID, userID, false)
		if err == nil && !ok {
			err = ycerrors.NotFound("Preferences", userID)
		}
		return err
	})
	return p, err
}

// verifiable returns the caller's channel c when a code can be sent to it.
func (s *Service) verifiable(ctx context.Context, c domain.Channel) (domain.MessagingChannel, string, string, error) {
	if c != domain.ChannelEmail && c != domain.ChannelSMS {
		return domain.MessagingChannel{}, "", "", ycerrors.Validation().
			Field("channel", "unsupported", "Only email and sms channels can be verified for now").Build()
	}
	var (
		mc               domain.MessagingChannel
		tenantID, userID string
	)
	err := s.asUser(ctx, func(tx pgx.Tx, tenant, user string) error {
		p, _, err := s.repo.Get(ctx, tx, tenant, user, false)
		if err != nil {
			return err
		}
		var ok bool
		if mc, ok = p.Channel(c); !ok {
			return ycerrors.NotFound("Channel", string(c))
		}
		tenantID, userID = tenant, user
		return nil
	})
	return mc, tenantID, userID, err
}

// ResendVerification asks yc-message for a new code for the caller's
// channel, returning when it expires.
func (s *Service) ResendVerification(ctx context.Context, c domain.Channel) (time.Time, error) {
	mc, _, userID, err := s.verifiable(ctx, c)
	if err != nil {
		return time.Time{}, err
	}
	if mc.Verified {
		return time.Time{}, ycerrors.Conflict("The channel is already verified")
	}
	v, err := s.messages.StartVerification(ctx, userID, string(c), mc.Address)
	return v.ExpiresAt, err
}

// VerifyChannel checks a code with yc-message and marks the caller's
// channel verified, publishing preference.channel.verified.v1.
func (s *Service) VerifyChannel(ctx context.Context, c domain.Channel, code string) (domain.MessagingChannel, error) {
	mc, tenantID, userID, err := s.verifiable(ctx, c)
	if err != nil || mc.Verified {
		return mc, err
	}
	v, err := s.messages.ConfirmVerification(ctx, userID, string(c), mc.Address, code)
	if err != nil {
		return domain.MessagingChannel{}, err
	}
	at := s.now()
	if v.ConfirmedAt != nil {
		at = *v.ConfirmedAt
	}
	return s.markVerified(ctx, tenantID, userID, c, mc.Address, at)
}

// ChannelVerified records a verification yc-message confirmed, unless the
// address has changed since.
func (s *Service) ChannelVerified(ctx context.Context, tenantID string, p events.MessageVerificationConfirmedV1) error {
	if err := validateShopTenant(tenantID); err != nil {
		return err
	}
	_, err := s.markVerified(ctx, tenantID, p.UserID, domain.Channel(p.Channel), p.Address, p.ConfirmedAt)
	if ycerrors.HasCode(err, ycerrors.CodeConflict) {
		return nil
	}
	return err
}

func (s *Service) markVerified(ctx context.Context, tenantID, userID string, c domain.Channel, address string, at time.Time) (domain.MessagingChannel, error) {
	var mc domain.MessagingChannel
	err := dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		p, _, err := s.repo.Get(ctx, tx, tenantID, userID, true)
		if err != nil {
			return err
		}
		cur, ok := p.Channel(c)
		if !ok || cur.Address != address {
			return ycerrors.Conflict("The channel's address changed; verify the new address")
		}
		if cur.Verified {
			mc = cur
			return nil
		}
		now := s.now()
		if err := s.repo.SetVerified(ctx, tx, tenantID, userID, c, true, &at, now); err != nil {
			return err
		}
		cur.Verified, cur.VerifiedAt, cur.UpdatedAt = true, &at, now
		mc = cur
		return s.publish(ctx, tx, tenantID, events.TypePreferenceChannelVerifiedV1, events.PreferenceChannelVerifiedV1{
			UserID: userID, Channel: string(c), Address: address, VerifiedAt: at.UTC(),
		})
	})
	return mc, err
}

// ChannelBounced unverifies an address yc-message could not deliver to.
// A primary channel other than email falls back to email (ADR-023 rule 5).
func (s *Service) ChannelBounced(ctx context.Context, tenantID string, p events.MessageBouncedV1) error {
	if p.UserID == nil {
		return nil
	}
	if err := validateShopTenant(tenantID); err != nil {
		return err
	}
	userID, c := *p.UserID, domain.Channel(p.Channel)
	return dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		prefs, ok, err := s.repo.Get(ctx, tx, tenantID, userID, true)
		if err != nil || !ok {
			return err
		}
		cur, ok := prefs.Channel(c)
		if !ok || cur.Address != p.Address {
			return nil
		}
		now := s.now()
		if cur.Verified {
			if err := s.repo.SetVerified(ctx, tx, tenantID, userID, c, false, nil, now); err != nil {
				return err
			}
		}
		if prefs.PrimaryChannel != c || c == domain.ChannelEmail {
			return nil
		}
		if _, ok := prefs.Channel(domain.ChannelEmail); !ok {
			return nil
		}
		prefs.PrimaryChannel = domain.ChannelEmail
		if err := s.repo.Update(ctx, tx, tenantID, prefs, now); err != nil {
			return err
		}
		return s.publishPrimary(ctx, tx, tenantID, userID, domain.ChannelEmail, c, now)
	})
}
