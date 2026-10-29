package service

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	"github.com/slaghuis/yc-platform/services/yc-support/internal/domain"
)

// Claim attaches the shop's unclaimed requests from email to userID, so a
// visitor who registers with the address they wrote from finds their
// threads under "my requests" (closed ones too, read-only). It returns how
// many requests it claimed.
//
// Only another service may call it, and the caller must have verified that
// userID controls email: anyone who can claim an address can read its
// threads.
func (s *Service) Claim(ctx context.Context, userID, email string) (int64, error) {
	a, _ := authctx.FromContext(ctx)
	if a.CallerService == "" {
		return 0, ycerrors.Forbidden("Service callers only").WithInternal("actor_type %s", a.ActorType)
	}
	u, err := uuid.Parse(userID)
	if err != nil {
		return 0, ycerrors.Validation().Field("user_id", "invalid", "User ID is not valid").Build()
	}
	email, err = domain.NormaliseEmail(email)
	if err != nil {
		return 0, ycerrors.Validation().Field("email", "invalid", "Enter a valid email address").Build()
	}
	var n int64
	err = s.inTenant(ctx, a.TenantID, func(tx pgx.Tx, tenantID string) error {
		var err error
		n, err = s.requests.Claim(ctx, tx, tenantID, u.String(), email, s.now().UTC())
		return err
	})
	return n, err
}
