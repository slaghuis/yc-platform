package service

import (
	"context"

	"github.com/jackc/pgx/v5"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/domain"
)

// GetMyBilling returns the caller's saved billing address.
func (s *Service) GetMyBilling(ctx context.Context) (domain.BillingAddress, error) {
	a, err := requireCustomer(ctx)
	if err != nil {
		return domain.BillingAddress{}, err
	}
	var b domain.BillingAddress
	err = dbkit.WithTenantTx(ctx, s.db, a.TenantID, func(tx pgx.Tx) error {
		var err error
		b, err = s.billing.Get(ctx, tx, a.ActorID)
		return err
	})
	return b, err
}

// PutMyBilling replaces the caller's billing address.
func (s *Service) PutMyBilling(ctx context.Context, in domain.BillingAddress) (domain.BillingAddress, error) {
	a, err := requireCustomer(ctx)
	if err != nil {
		return domain.BillingAddress{}, err
	}
	in = domain.NormaliseBilling(in)
	if err := domain.ValidateBilling(in); err != nil {
		return domain.BillingAddress{}, err
	}
	err = dbkit.WithTenantTx(ctx, s.db, a.TenantID, func(tx pgx.Tx) error {
		user, err := s.users.Get(ctx, tx, a.ActorID)
		if err != nil {
			return err
		}
		if user.Status != domain.StatusActive {
			return ycerrors.Forbidden("Account is not active")
		}
		return s.billing.Put(ctx, tx, a.TenantID, a.ActorID, in, s.now().UTC())
	})
	return in, err
}

// DeleteMyBilling clears the caller's billing address.
func (s *Service) DeleteMyBilling(ctx context.Context) error {
	a, err := requireCustomer(ctx)
	if err != nil {
		return err
	}
	return dbkit.WithTenantTx(ctx, s.db, a.TenantID, func(tx pgx.Tx) error {
		return s.billing.Delete(ctx, tx, a.ActorID)
	})
}

// UserBilling returns a customer's billing for service callers (checkout/orders).
func (s *Service) UserBilling(ctx context.Context, userID string) (domain.BillingAddress, error) {
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorService {
		return domain.BillingAddress{}, ycerrors.Forbidden("Service callers only")
	}
	if err := validateShopTenant(a.TenantID); err != nil {
		return domain.BillingAddress{}, err
	}
	var b domain.BillingAddress
	err := dbkit.WithTenantTx(ctx, s.db, a.TenantID, func(tx pgx.Tx) error {
		var err error
		b, err = s.billing.Get(ctx, tx, userID)
		return err
	})
	return b, err
}

func requireCustomer(ctx context.Context) (authctx.AuthContext, error) {
	a, _ := authctx.FromContext(ctx)
	if !a.Authenticated() {
		return a, ycerrors.Unauthorized("Authentication required")
	}
	if a.ActorType != authctx.ActorCustomer {
		return a, ycerrors.Forbidden("Customers only").WithInternal("actor_type %s", a.ActorType)
	}
	if err := validateShopTenant(a.TenantID); err != nil {
		return a, err
	}
	return a, nil
}
