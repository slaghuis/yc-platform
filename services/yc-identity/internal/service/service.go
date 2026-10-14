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

// Service implements visitor issuance, token validation and profile reads.
type Service struct {
	db     *pgxpool.Pool
	bus    Publisher
	signer *token.Signer
	users  repository.Users
	cfg    Config
}

// New returns a Service.
func New(db *pgxpool.Pool, bus Publisher, signer *token.Signer, cfg Config) *Service {
	return &Service{db: db, bus: bus, signer: signer, cfg: cfg}
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

// ValidateToken verifies raw and checks that its user is still active.
// Every rejection is an unauthorized error with no detail for the client.
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

func (s *Service) getUser(ctx context.Context, tenantID, userID string) (domain.User, error) {
	var user domain.User
	get := func(tx pgx.Tx) error {
		var err error
		user, err = s.users.Get(ctx, tx, userID)
		return err
	}
	var err error
	if tenantID == dbkit.PlatformTenantID {
		err = dbkit.WithPlatformTx(ctx, s.db, get)
	} else {
		err = dbkit.WithTenantTx(ctx, s.db, tenantID, get)
	}
	return user, err
}

func validateShopTenant(tenantID string) error {
	if tenantID == "" {
		return ycerrors.Validation().Field("tenant_id", "required", "Tenant is required").Build()
	}
	if tenantID == eventbus.PlatformTenantID {
		return ycerrors.Validation().Field("tenant_id", "invalid", "Visitors belong to a shop").Build()
	}
	if err := eventbus.ValidateTenantID(tenantID); err != nil {
		return ycerrors.Validation().Field("tenant_id", "invalid", "Tenant ID is not valid").Wrap(err)
	}
	return nil
}
