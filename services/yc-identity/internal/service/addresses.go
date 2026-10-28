package service

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/domain"
)

// ListMyAddresses returns the caller's address book.
func (s *Service) ListMyAddresses(ctx context.Context, includeDeleted bool) ([]domain.DeliveryAddress, error) {
	a, err := requireCustomer(ctx)
	if err != nil {
		return nil, err
	}
	var out []domain.DeliveryAddress
	err = dbkit.WithTenantTx(ctx, s.db, a.TenantID, func(tx pgx.Tx) error {
		var err error
		out, err = s.addresses.List(ctx, tx, a.ActorID, includeDeleted)
		return err
	})
	return out, err
}

// GetMyAddress returns one of the caller's addresses (including soft-deleted).
func (s *Service) GetMyAddress(ctx context.Context, id string) (domain.DeliveryAddress, error) {
	a, err := requireCustomer(ctx)
	if err != nil {
		return domain.DeliveryAddress{}, err
	}
	var out domain.DeliveryAddress
	err = dbkit.WithTenantTx(ctx, s.db, a.TenantID, func(tx pgx.Tx) error {
		var err error
		out, err = s.addresses.Get(ctx, tx, a.ActorID, id)
		return err
	})
	return out, err
}

// CreateMyAddress adds an active book entry.
func (s *Service) CreateMyAddress(ctx context.Context, in domain.DeliveryAddressInput) (domain.DeliveryAddress, error) {
	a, err := requireCustomer(ctx)
	if err != nil {
		return domain.DeliveryAddress{}, err
	}
	return s.createAddress(ctx, a.TenantID, a.ActorID, in)
}

// PutMyAddress soft-retires id and inserts a successor (issue #32).
func (s *Service) PutMyAddress(ctx context.Context, id string, in domain.DeliveryAddressInput) (domain.DeliveryAddress, error) {
	a, err := requireCustomer(ctx)
	if err != nil {
		return domain.DeliveryAddress{}, err
	}
	in = domain.NormaliseDeliveryAddressInput(in)
	if err := domain.ValidateDeliveryAddressInput(in); err != nil {
		return domain.DeliveryAddress{}, err
	}
	if err := s.requireAddressNotInUse(ctx, a.TenantID, id); err != nil {
		return domain.DeliveryAddress{}, err
	}

	now := s.now().UTC()
	var out domain.DeliveryAddress
	err = dbkit.WithTenantTx(ctx, s.db, a.TenantID, func(tx pgx.Tx) error {
		user, err := s.users.Get(ctx, tx, a.ActorID)
		if err != nil {
			return err
		}
		if user.Status != domain.StatusActive {
			return ycerrors.Forbidden("Account is not active")
		}
		cur, err := s.addresses.GetActive(ctx, tx, a.ActorID, id)
		if err != nil {
			return err
		}
		wantDefault := in.IsDefault || cur.IsDefault
		if wantDefault {
			if err := s.addresses.ClearDefault(ctx, tx, a.ActorID, now); err != nil {
				return err
			}
		}
		out = domain.DeliveryAddress{
			ID: uuid.NewString(), UserID: a.ActorID, Label: in.Label, Address: in.Address,
			IsDefault: wantDefault, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.addresses.Insert(ctx, tx, a.TenantID, a.ActorID, out); err != nil {
			return err
		}
		return s.addresses.SoftDelete(ctx, tx, a.ActorID, id, out.ID, now)
	})
	return out, err
}

// DeleteMyAddress soft-retires an address.
func (s *Service) DeleteMyAddress(ctx context.Context, id string) error {
	a, err := requireCustomer(ctx)
	if err != nil {
		return err
	}
	if err := s.requireAddressNotInUse(ctx, a.TenantID, id); err != nil {
		return err
	}
	now := s.now().UTC()
	return dbkit.WithTenantTx(ctx, s.db, a.TenantID, func(tx pgx.Tx) error {
		cur, err := s.addresses.GetActive(ctx, tx, a.ActorID, id)
		if err != nil {
			return err
		}
		if err := s.addresses.SoftDelete(ctx, tx, a.ActorID, id, "", now); err != nil {
			return err
		}
		if cur.IsDefault {
			return s.addresses.PromoteOldestDefault(ctx, tx, a.ActorID, now)
		}
		return nil
	})
}

// ListUserAddresses is shop-admin read-only of a customer's book.
func (s *Service) ListUserAddresses(ctx context.Context, userID string, includeDeleted bool) ([]domain.DeliveryAddress, error) {
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorShopAdmin && a.ActorType != authctx.ActorSystemAdmin {
		return nil, ycerrors.Forbidden("Shop admins only")
	}
	if err := validateShopTenant(a.TenantID); err != nil && a.ActorType != authctx.ActorSystemAdmin {
		return nil, err
	}
	tenantID := a.TenantID
	var out []domain.DeliveryAddress
	err := dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		user, err := s.users.Get(ctx, tx, userID)
		if err != nil {
			return err
		}
		if a.ActorType == authctx.ActorShopAdmin && user.TenantID != a.TenantID {
			return ycerrors.NotFound("User", userID)
		}
		out, err = s.addresses.List(ctx, tx, userID, includeDeleted)
		return err
	})
	return out, err
}

// GetUserAddress is shop-admin read of one address.
func (s *Service) GetUserAddress(ctx context.Context, userID, id string) (domain.DeliveryAddress, error) {
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorShopAdmin && a.ActorType != authctx.ActorSystemAdmin {
		return domain.DeliveryAddress{}, ycerrors.Forbidden("Shop admins only")
	}
	var out domain.DeliveryAddress
	err := dbkit.WithTenantTx(ctx, s.db, a.TenantID, func(tx pgx.Tx) error {
		user, err := s.users.Get(ctx, tx, userID)
		if err != nil {
			return err
		}
		if a.ActorType == authctx.ActorShopAdmin && user.TenantID != a.TenantID {
			return ycerrors.NotFound("User", userID)
		}
		out, err = s.addresses.Get(ctx, tx, userID, id)
		return err
	})
	return out, err
}

// UserAddress returns an active address for service callers (checkout).
func (s *Service) UserAddress(ctx context.Context, userID, id string) (domain.DeliveryAddress, error) {
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorService {
		return domain.DeliveryAddress{}, ycerrors.Forbidden("Service callers only")
	}
	if err := validateShopTenant(a.TenantID); err != nil {
		return domain.DeliveryAddress{}, err
	}
	var out domain.DeliveryAddress
	err := dbkit.WithTenantTx(ctx, s.db, a.TenantID, func(tx pgx.Tx) error {
		var err error
		out, err = s.addresses.GetActive(ctx, tx, userID, id)
		return err
	})
	return out, err
}

// CreateUserAddress creates a book entry for service callers (checkout auto-save).
func (s *Service) CreateUserAddress(ctx context.Context, userID string, in domain.DeliveryAddressInput) (domain.DeliveryAddress, error) {
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorService {
		return domain.DeliveryAddress{}, ycerrors.Forbidden("Service callers only")
	}
	if err := validateShopTenant(a.TenantID); err != nil {
		return domain.DeliveryAddress{}, err
	}
	return s.createAddress(ctx, a.TenantID, userID, in)
}

func (s *Service) createAddress(ctx context.Context, tenantID, userID string, in domain.DeliveryAddressInput) (domain.DeliveryAddress, error) {
	in = domain.NormaliseDeliveryAddressInput(in)
	if err := domain.ValidateDeliveryAddressInput(in); err != nil {
		return domain.DeliveryAddress{}, err
	}
	now := s.now().UTC()
	var out domain.DeliveryAddress
	err := dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		user, err := s.users.Get(ctx, tx, userID)
		if err != nil {
			return err
		}
		if user.Status != domain.StatusActive {
			return ycerrors.Forbidden("Account is not active")
		}
		n, err := s.addresses.CountActive(ctx, tx, userID)
		if err != nil {
			return err
		}
		if n >= domain.MaxDeliveryAddresses {
			return ycerrors.Validation().Field("address", "invalid",
				"At most 5 delivery addresses can be saved").Build()
		}
		isDefault := in.IsDefault || n == 0
		if isDefault {
			if err := s.addresses.ClearDefault(ctx, tx, userID, now); err != nil {
				return err
			}
		}
		out = domain.DeliveryAddress{
			ID: uuid.NewString(), UserID: userID, Label: in.Label, Address: in.Address,
			IsDefault: isDefault, CreatedAt: now, UpdatedAt: now,
		}
		return s.addresses.Insert(ctx, tx, tenantID, userID, out)
	})
	return out, err
}

func (s *Service) requireAddressNotInUse(ctx context.Context, tenantID, addressID string) error {
	if s.cfg.Orders == nil {
		return nil
	}
	a, _ := authctx.FromContext(ctx)
	inTenant := authctx.WithAuth(ctx, authctx.AuthContext{
		TenantID: tenantID, ActorID: a.ActorID, ActorType: a.ActorType, CorrelationID: a.CorrelationID,
	})
	inUse, err := s.cfg.Orders.AddressInUse(inTenant, addressID)
	if err != nil {
		return ycerrors.Unavailable("The address cannot be changed right now; try again later").Wrap(err)
	}
	if inUse {
		return ycerrors.Conflict("This address is used by an order still in progress").
			With("delivery_address_id", addressID).
			With("reason", "address_in_use")
	}
	return nil
}
