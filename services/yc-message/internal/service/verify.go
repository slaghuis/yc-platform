package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	"github.com/slaghuis/yc-platform/services/yc-message/internal/domain"
)

// VerifyRequest names a user's channel address to verify.
type VerifyRequest struct {
	UserID  string
	Channel domain.Channel
	Address string
}

func (r VerifyRequest) validate() error {
	v := ycerrors.Validation()
	if r.UserID == "" {
		v.Field("user_id", "required", "User ID is required")
	}
	if r.Channel != domain.ChannelEmail && r.Channel != domain.ChannelSMS {
		v.Field("channel", "unsupported", "Only email and sms channels can be verified")
	}
	if strings.TrimSpace(r.Address) == "" {
		v.Field("address", "required", "Address is required")
	}
	if v.HasErrors() {
		return v.Build()
	}
	return nil
}

// ChannelAdded sends a code to a newly added channel address.
func (s *Service) ChannelAdded(ctx context.Context, tenantID, eventID string, p events.PreferenceChannelAddedV1) error {
	r := VerifyRequest{UserID: p.UserID, Channel: domain.Channel(p.Channel), Address: p.Address}
	if !s.hasDriver(r.Channel) || (r.Channel != domain.ChannelEmail && r.Channel != domain.ChannelSMS) {
		return nil
	}
	_, err := s.startVerification(serviceContext(ctx, tenantID), tenantID, r, "event:"+eventID)
	return err
}

// StartVerification sends a new code to a channel address on yc-preference's
// request. Earlier codes for the channel stop working. A code for the same
// address can be resent once a minute.
func (s *Service) StartVerification(ctx context.Context, r VerifyRequest) (domain.Verification, error) {
	if err := r.validate(); err != nil {
		return domain.Verification{}, err
	}
	return s.startVerification(ctx, callerTenant(ctx), r, "")
}

func (s *Service) startVerification(ctx context.Context, tenantID string, r VerifyRequest, reference string) (domain.Verification, error) {
	locale := s.cfg.Locale
	if p, err := s.upstream.Preferences(ctx, r.UserID); err == nil && p.Locale != "" {
		locale = p.Locale
	}
	var v domain.Verification
	err := s.inTenant(ctx, tenantID, func(tx pgx.Tx, tenantID string) error {
		now := s.now().UTC()
		latest, ok, err := s.repo.LatestVerification(ctx, tx, tenantID, r.UserID, r.Channel)
		if err != nil {
			return err
		}
		if reference != "" {
			if _, sent, err := s.repo.MessageByReference(ctx, tx, tenantID, reference); err != nil || sent {
				v = latest
				return err
			}
		}
		if ok && latest.Address == r.Address && latest.ConfirmedAt == nil && now.Sub(latest.CreatedAt) < domain.VerificationResend {
			return ycerrors.RateLimited("A code was sent less than a minute ago")
		}
		code, err := domain.NewCode()
		if err != nil {
			return err
		}
		v = domain.Verification{
			ID: uuid.NewString(), TenantID: tenantID, UserID: r.UserID, Channel: r.Channel, Address: r.Address,
			ExpiresAt: now.Add(s.cfg.VerificationTTL), CreatedAt: now,
		}
		v.CodeHash = domain.HashCode(v.ID, code)
		if err := s.repo.InsertVerification(ctx, tx, v); err != nil {
			return err
		}
		if reference == "" {
			reference = "verification:" + v.ID
		}
		if _, err := s.queue(ctx, tx, tenantID, request{
			Type: domain.TypeChannelVerification, Reference: reference,
			To:      domain.Recipient{UserID: r.UserID, Channel: r.Channel, Address: r.Address, Locale: locale},
			Context: map[string]any{"code": code, "expires_minutes": int(s.cfg.VerificationTTL / time.Minute)},
		}); err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypeMessageVerificationSentV1, events.MessageVerificationSentV1{
			VerificationID: v.ID, UserID: v.UserID, Channel: string(v.Channel), ExpiresAt: v.ExpiresAt, SentAt: now,
		})
	})
	return v, err
}

// ConfirmVerification checks a code against the latest one sent to the
// channel. A wrong code uses up one of its attempts. Confirming an already
// confirmed address again succeeds.
func (s *Service) ConfirmVerification(ctx context.Context, r VerifyRequest, code string) (domain.Verification, error) {
	if err := r.validate(); err != nil {
		return domain.Verification{}, err
	}
	var (
		v     domain.Verification
		wrong bool
	)
	err := s.inShop(ctx, func(tx pgx.Tx, tenantID string) error {
		var (
			ok  bool
			err error
		)
		v, ok, err = s.repo.LatestVerification(ctx, tx, tenantID, r.UserID, r.Channel)
		if err != nil {
			return err
		}
		if !ok || v.Address != r.Address {
			return ycerrors.Conflict("No code was sent to this address")
		}
		if v.ConfirmedAt != nil {
			return nil
		}
		now := s.now().UTC()
		if !v.Usable(now) {
			return ycerrors.Conflict("The code has expired or was tried too often; request a new one")
		}
		if !v.Matches(code) {
			wrong = true
			v.Attempts++
			return s.repo.UpdateVerification(ctx, tx, v)
		}
		v.ConfirmedAt = &now
		if err := s.repo.UpdateVerification(ctx, tx, v); err != nil {
			return err
		}
		return s.publish(ctx, tx, tenantID, events.TypeMessageVerificationConfirmedV1, events.MessageVerificationConfirmedV1{
			VerificationID: v.ID, UserID: v.UserID, Channel: string(v.Channel), Address: v.Address, ConfirmedAt: now,
		})
	})
	if err == nil && wrong {
		err = ycerrors.Validation().Field("code", "invalid",
			fmt.Sprintf("The code is not correct; %d attempts left", domain.MaxCodeAttempts-v.Attempts)).Build()
	}
	return v, err
}
