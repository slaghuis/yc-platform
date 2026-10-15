// Package service holds yc-identity's use cases.
package service

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/password"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/repository"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/token"
)

// Publisher writes events to the outbox in the caller's transaction.
type Publisher interface {
	Publish(ctx context.Context, tx pgx.Tx, evt eventbus.Event) error
}

// Config holds the token lifetimes.
type Config struct {
	AccessTokenTTL  time.Duration
	VisitorTokenTTL time.Duration
}

// Service implements visitor issuance, registration, login, token
// validation and user administration.
type Service struct {
	db     *pgxpool.Pool
	bus    Publisher
	signer *token.Signer
	hasher *password.Hasher
	users  repository.Users
	cfg    Config
	now    func() time.Time
}

// New returns a Service.
func New(db *pgxpool.Pool, bus Publisher, signer *token.Signer, hasher *password.Hasher, cfg Config) *Service {
	return &Service{db: db, bus: bus, signer: signer, hasher: hasher, cfg: cfg, now: time.Now}
}

// CreateVisitor creates an anonymous visitor in tenantID, issues its token
// and publishes identity.visitor.created.v1, atomically.
func (s *Service) CreateVisitor(ctx context.Context, tenantID string) (domain.Session, error) {
	if err := validateShopTenant(tenantID); err != nil {
		return domain.Session{}, err
	}

	var sess domain.Session
	err := dbkit.WithTenantTx(ctx, s.db, tenantID, func(tx pgx.Tx) error {
		user, err := s.users.CreateVisitor(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		if sess, err = s.signer.Issue(user, s.cfg.VisitorTokenTTL); err != nil {
			return err
		}
		jti := sess.TokenID
		return s.bus.Publish(ctx, tx, eventbus.Event{
			EventType: events.TypeIdentityVisitorCreatedV1,
			TenantID:  tenantID,
			Actor:     eventbus.Actor{Type: string(authctx.ActorVisitor), ID: user.ID, AccessTokenJTI: jti},
			Payload: events.IdentityVisitorCreatedV1{
				UserID:         user.ID,
				CreatedAt:      user.CreatedAt.UTC(),
				TokenExpiresAt: sess.ExpiresAt.UTC(),
			},
		})
	})
	if err != nil {
		return domain.Session{}, fmt.Errorf("create visitor: %w", err)
	}
	return sess, nil
}

// Register makes the calling visitor a customer with email and password,
// keeping its user ID, issues a customer token and publishes
// identity.user.registered.v1. A caller without a visitor (or whose
// visitor is gone) gets a new customer.
func (s *Service) Register(ctx context.Context, reg domain.Registration) (domain.Session, error) {
	a, _ := authctx.FromContext(ctx)
	if err := validateShopTenant(a.TenantID); err != nil {
		return domain.Session{}, err
	}
	if a.Authenticated() && a.ActorType != authctx.ActorVisitor {
		return domain.Session{}, ycerrors.Conflict("Already registered").
			WithInternal("actor %s is %s", a.ActorID, a.ActorType)
	}
	hash, err := s.hasher.Hash(reg.Password)
	if err != nil {
		return domain.Session{}, err
	}

	var sess domain.Session
	err = dbkit.WithTenantTx(ctx, s.db, a.TenantID, func(tx pgx.Tx) error {
		user, err := s.registerUser(ctx, tx, a, reg, hash)
		if err != nil {
			return err
		}
		if sess, err = s.signer.Issue(user, s.cfg.AccessTokenTTL); err != nil {
			return err
		}
		var displayName *string
		if user.DisplayName != "" {
			displayName = &user.DisplayName
		}
		return s.bus.Publish(ctx, tx, eventbus.Event{
			EventType: events.TypeIdentityUserRegisteredV1,
			TenantID:  user.TenantID,
			Actor:     eventbus.Actor{Type: string(authctx.ActorCustomer), ID: user.ID, AccessTokenJTI: sess.TokenID},
			Payload: events.IdentityUserRegisteredV1{
				UserID:       user.ID,
				Email:        user.Email,
				DisplayName:  displayName,
				Role:         events.IdentityUserRegisteredV1RoleCustomer,
				Method:       events.IdentityUserRegisteredV1MethodPassword,
				RegisteredAt: s.now().UTC(),
			},
		})
	})
	if err != nil {
		return domain.Session{}, fmt.Errorf("register: %w", err)
	}
	return sess, nil
}

func (s *Service) registerUser(ctx context.Context, tx pgx.Tx, a authctx.AuthContext, reg domain.Registration, hash string) (domain.User, error) {
	if a.ActorType == authctx.ActorVisitor {
		user, err := s.users.Register(ctx, tx, a.ActorID, reg, hash)
		if !ycerrors.HasCode(err, ycerrors.CodeNotFound) {
			return user, err
		}
	}
	return s.users.Create(ctx, tx, a.TenantID, domain.RoleCustomer, reg, hash)
}

// errBadCredentials is the only login failure a client sees, whatever the
// cause, so login cannot be used to discover registered emails.
func errBadCredentials(internal string, args ...any) error {
	return ycerrors.Unauthorized("Invalid email or password").WithInternal(internal, args...)
}

// Login checks email and password in the tenant yc-gateway resolved and
// issues a token for the user's role. It publishes
// identity.user.logged_in.v1 with the calling visitor, if any, as the cart
// merge source.
func (s *Service) Login(ctx context.Context, email, pw string) (domain.Session, error) {
	a, _ := authctx.FromContext(ctx)
	if a.TenantID == "" {
		return domain.Session{}, ycerrors.Validation().Field("tenant_id", "required", "Tenant is required").Build()
	}
	if err := eventbus.ValidateTenantID(a.TenantID); err != nil {
		return domain.Session{}, ycerrors.Validation().Field("tenant_id", "invalid", "Tenant ID is not valid").Wrap(err)
	}
	norm, ok := domain.NormaliseEmail(email)
	if !ok {
		s.hasher.VerifyNothing(pw)
		return domain.Session{}, errBadCredentials("malformed email")
	}

	var user domain.User
	err := s.inTenant(ctx, a.TenantID, func(tx pgx.Tx) error {
		var err error
		user, err = s.users.GetByEmail(ctx, tx, a.TenantID, norm)
		return err
	})
	if ycerrors.HasCode(err, ycerrors.CodeNotFound) || (err == nil && user.PasswordHash == "") {
		s.hasher.VerifyNothing(pw)
		return domain.Session{}, errBadCredentials("no user with a password for this email")
	}
	if err != nil {
		return domain.Session{}, err
	}
	match, err := s.hasher.Verify(pw, user.PasswordHash)
	if err != nil {
		return domain.Session{}, fmt.Errorf("login: verify user %s: %w", user.ID, err)
	}
	if !match {
		return domain.Session{}, errBadCredentials("wrong password for user %s", user.ID)
	}
	if user.Status != domain.StatusActive {
		return domain.Session{}, errBadCredentials("user %s is %s", user.ID, user.Status)
	}

	sess, err := s.signer.Issue(user, s.cfg.AccessTokenTTL)
	if err != nil {
		return domain.Session{}, err
	}
	var previousVisitor *string
	if a.ActorType == authctx.ActorVisitor && a.ActorID != "" && a.ActorID != user.ID {
		previousVisitor = &a.ActorID
	}
	err = s.inTenant(ctx, a.TenantID, func(tx pgx.Tx) error {
		return s.bus.Publish(ctx, tx, eventbus.Event{
			EventType: events.TypeIdentityUserLoggedInV1,
			TenantID:  user.TenantID,
			Actor:     eventbus.Actor{Type: string(user.Role.ActorType()), ID: user.ID, AccessTokenJTI: sess.TokenID},
			Payload: events.IdentityUserLoggedInV1{
				UserID:            user.ID,
				Role:              string(user.Role),
				Method:            events.IdentityUserLoggedInV1MethodPassword,
				PreviousVisitorID: previousVisitor,
				LoggedInAt:        s.now().UTC(),
			},
		})
	})
	if err != nil {
		return domain.Session{}, fmt.Errorf("login: %w", err)
	}
	return sess, nil
}

// ValidateToken verifies raw and checks that its user is still active and
// still has the token's role. Every rejection is an unauthorized error with
// no detail for the client.
func (s *Service) ValidateToken(ctx context.Context, raw string) (domain.Session, error) {
	sess, err := s.signer.Verify(raw)
	if err != nil {
		return domain.Session{}, ycerrors.Unauthorized("Invalid or expired token").Wrap(err)
	}
	user, err := s.getUser(ctx, sess.TenantID, sess.UserID)
	if ycerrors.HasCode(err, ycerrors.CodeNotFound) {
		return domain.Session{}, ycerrors.Unauthorized("Invalid or expired token").
			WithInternal("user %s no longer exists", sess.UserID)
	}
	if err != nil {
		return domain.Session{}, err
	}
	if user.Status != domain.StatusActive {
		return domain.Session{}, ycerrors.Unauthorized("Invalid or expired token").
			WithInternal("user %s is %s", user.ID, user.Status)
	}
	if user.Role != sess.Role {
		return domain.Session{}, ycerrors.Unauthorized("Invalid or expired token").
			WithInternal("user %s is now %s, token says %s", user.ID, user.Role, sess.Role)
	}
	return sess, nil
}

// Me returns the authenticated caller's user.
func (s *Service) Me(ctx context.Context) (domain.User, error) {
	a, _ := authctx.FromContext(ctx)
	if !a.Authenticated() {
		return domain.User{}, ycerrors.Unauthorized("Authentication required")
	}
	return s.getUser(ctx, a.TenantID, a.ActorID)
}

// MaxPageSize bounds ListUsers.
const MaxPageSize = 100

// ListUsers returns a page of users, newest first. Shop admins see only
// their tenant; system admins see every tenant unless f.TenantID is set.
// The returned cursor is nil on the last page.
func (s *Service) ListUsers(ctx context.Context, f repository.Filter, after *repository.Cursor, limit int) ([]domain.User, *repository.Cursor, error) {
	a, _ := authctx.FromContext(ctx)
	scope, err := adminScope(a, f.TenantID)
	if err != nil {
		return nil, nil, err
	}
	f.TenantID = scope
	limit = min(max(limit, 1), MaxPageSize)

	var users []domain.User
	err = s.inTenant(ctx, a.TenantID, func(tx pgx.Tx) error {
		var err error
		users, err = s.users.List(ctx, tx, f, after, limit+1)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	var next *repository.Cursor
	if len(users) > limit {
		users = users[:limit]
		last := users[limit-1]
		next = &repository.Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return users, next, nil
}

// GetUser returns user id, within the caller's tenant for shop admins.
func (s *Service) GetUser(ctx context.Context, id string) (domain.User, error) {
	a, _ := authctx.FromContext(ctx)
	if _, err := adminScope(a, ""); err != nil {
		return domain.User{}, err
	}
	return s.getUser(ctx, a.TenantID, id)
}

// Promote raises user id to role in its own tenant. Only system admins may
// promote (docs/services/yc-identity.md, Role Hierarchy); only customers
// can become shop admins.
func (s *Service) Promote(ctx context.Context, id string, role domain.Role) (domain.User, error) {
	a, _ := authctx.FromContext(ctx)
	if a.ActorType != authctx.ActorSystemAdmin {
		return domain.User{}, ycerrors.Forbidden("Only system admins can promote users")
	}
	if role != domain.RoleShopAdmin {
		return domain.User{}, ycerrors.Validation().Field("role", "invalid", "Users can only be promoted to shop_admin").Build()
	}

	// RLS lets a _platform transaction read every tenant but write none,
	// so find the user's tenant first and change it there.
	found, err := s.getUser(ctx, dbkit.PlatformTenantID, id)
	if err != nil {
		return domain.User{}, err
	}
	if found.TenantID == dbkit.PlatformTenantID {
		return domain.User{}, ycerrors.Conflict("System admins cannot be promoted")
	}

	var promoted domain.User
	err = dbkit.WithTenantTx(ctx, s.db, found.TenantID, func(tx pgx.Tx) error {
		user, err := s.users.GetForUpdate(ctx, tx, id)
		if err != nil {
			return err
		}
		switch {
		case user.Role == domain.RoleVisitor:
			return ycerrors.Validation().Message("Visitors must register before promotion").Build()
		case !role.Above(user.Role):
			return ycerrors.Conflict(fmt.Sprintf("User is already %s", user.Role))
		case user.Status != domain.StatusActive:
			return ycerrors.Conflict(fmt.Sprintf("User is %s", user.Status))
		}
		if promoted, err = s.users.SetRole(ctx, tx, id, role); err != nil {
			return err
		}
		return s.bus.Publish(ctx, tx, eventbus.Event{
			EventType: events.TypeIdentityUserPromotedV1,
			TenantID:  user.TenantID,
			Actor:     eventbus.Actor{Type: string(a.ActorType), ID: a.ActorID},
			Payload: events.IdentityUserPromotedV1{
				UserID:     user.ID,
				FromRole:   string(user.Role),
				ToRole:     string(role),
				PromotedAt: s.now().UTC(),
			},
		})
	})
	if err != nil {
		return domain.User{}, fmt.Errorf("promote user %s: %w", id, err)
	}
	return promoted, nil
}

// BootstrapSystemAdmin creates a system admin with email and password if
// none has that email yet. It reports whether a user was created; an
// existing one is left unchanged.
func (s *Service) BootstrapSystemAdmin(ctx context.Context, email, pw, displayName string) (domain.User, bool, error) {
	reg, err := domain.NewRegistration(email, pw, displayName)
	if err != nil {
		return domain.User{}, false, err
	}
	hash, err := s.hasher.Hash(reg.Password)
	if err != nil {
		return domain.User{}, false, err
	}
	var (
		user    domain.User
		created bool
	)
	err = dbkit.WithPlatformTx(ctx, s.db, func(tx pgx.Tx) error {
		existing, err := s.users.GetByEmail(ctx, tx, dbkit.PlatformTenantID, reg.Email)
		if err == nil {
			user = existing
			return nil
		}
		if !ycerrors.HasCode(err, ycerrors.CodeNotFound) {
			return err
		}
		user, err = s.users.Create(ctx, tx, dbkit.PlatformTenantID, domain.RoleSystemAdmin, reg, hash)
		created = err == nil
		return err
	})
	if err != nil {
		return domain.User{}, false, fmt.Errorf("bootstrap system admin: %w", err)
	}
	return user, created, nil
}

// adminScope returns the tenant an admin request is confined to: the
// caller's own for shop admins, requested (possibly "" for all) for system
// admins.
func adminScope(a authctx.AuthContext, requested string) (string, error) {
	switch a.ActorType {
	case authctx.ActorSystemAdmin:
		return requested, nil
	case authctx.ActorShopAdmin:
		if requested != "" && requested != a.TenantID {
			return "", ycerrors.Forbidden("Shop admins see only their own shop").
				WithInternal("shop admin of %s asked for %s", a.TenantID, requested)
		}
		return a.TenantID, nil
	case authctx.ActorVisitor, authctx.ActorCustomer, authctx.ActorService:
		return "", ycerrors.Forbidden("Role not permitted")
	default:
		return "", ycerrors.Forbidden("Role not permitted")
	}
}

func (s *Service) getUser(ctx context.Context, tenantID, userID string) (domain.User, error) {
	var user domain.User
	err := s.inTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		user, err = s.users.Get(ctx, tx, userID)
		return err
	})
	return user, err
}

// inTenant runs fn in tenantID's RLS context; _platform sees every tenant.
func (s *Service) inTenant(ctx context.Context, tenantID string, fn dbkit.TxFunc) error {
	if tenantID == dbkit.PlatformTenantID {
		return dbkit.WithPlatformTx(ctx, s.db, fn)
	}
	return dbkit.WithTenantTx(ctx, s.db, tenantID, fn)
}

func validateShopTenant(tenantID string) error {
	if tenantID == "" {
		return ycerrors.Validation().Field("tenant_id", "required", "Tenant is required").Build()
	}
	if tenantID == eventbus.PlatformTenantID {
		return ycerrors.Validation().Field("tenant_id", "invalid", "Visitors and customers belong to a shop").Build()
	}
	if err := eventbus.ValidateTenantID(tenantID); err != nil {
		return ycerrors.Validation().Field("tenant_id", "invalid", "Tenant ID is not valid").Wrap(err)
	}
	return nil
}
