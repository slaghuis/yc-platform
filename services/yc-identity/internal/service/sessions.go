package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/repository"
)

// newSecret returns an opaque random token and the hash it is stored by.
func newSecret() (string, []byte) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	raw := base64.RawURLEncoding.EncodeToString(b)
	return raw, hashSecret(raw)
}

func hashSecret(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

// issueSession signs an access token for user and, unless user is a
// visitor, stores a refresh token in familyID ("" starts a new login). It
// returns the refresh token's row ID.
func (s *Service) issueSession(ctx context.Context, tx pgx.Tx, user domain.User, familyID string) (domain.Session, string, error) {
	ttl := s.cfg.AccessTokenTTL
	if user.Role == domain.RoleVisitor {
		ttl = s.cfg.VisitorTokenTTL
	}
	sess, err := s.signer.Issue(user, ttl)
	if err != nil || user.Role == domain.RoleVisitor {
		return sess, "", err
	}
	if familyID == "" {
		familyID = uuid.NewString()
	}
	raw, hash := newSecret()
	expires := s.now().Add(s.cfg.RefreshTokenTTL)
	id, err := s.sessions.InsertRefresh(ctx, tx, repository.RefreshToken{
		TenantID: user.TenantID, UserID: user.ID, FamilyID: familyID, ExpiresAt: expires,
	}, hash)
	if err != nil {
		return domain.Session{}, "", err
	}
	sess.RefreshToken, sess.RefreshExpiresAt = raw, expires
	return sess, id, nil
}

func errBadRefresh(internal string, args ...any) error {
	return ycerrors.Unauthorized("Invalid or expired refresh token").WithInternal(internal, args...)
}

// Refresh rotates raw into a new session for the tenant yc-gateway
// resolved. A token that was already rotated or revoked revokes its whole
// login (reuse detection).
func (s *Service) Refresh(ctx context.Context, raw string) (domain.Session, error) {
	a, _ := authctx.FromContext(ctx)
	if err := validateTenant(a.TenantID); err != nil {
		return domain.Session{}, err
	}
	var (
		sess   domain.Session
		reused string
	)
	err := s.inTenant(ctx, a.TenantID, func(tx pgx.Tx) error {
		rt, err := s.sessions.GetRefreshForUpdate(ctx, tx, hashSecret(raw))
		if ycerrors.HasCode(err, ycerrors.CodeNotFound) {
			return errBadRefresh("unknown refresh token")
		}
		if err != nil {
			return err
		}
		if rt.TenantID != a.TenantID {
			return errBadRefresh("refresh token of %s presented to %s", rt.TenantID, a.TenantID)
		}
		if rt.RevokedAt != nil || rt.ReplacedBy != nil {
			reused = rt.FamilyID
			return s.sessions.RevokeFamily(ctx, tx, rt.FamilyID)
		}
		if !s.now().Before(rt.ExpiresAt) {
			return errBadRefresh("refresh token %s expired", rt.ID)
		}
		user, err := s.users.Get(ctx, tx, rt.UserID)
		if err != nil {
			return err
		}
		if user.Status != domain.StatusActive {
			return errBadRefresh("user %s is %s", user.ID, user.Status)
		}
		var next string
		if sess, next, err = s.issueSession(ctx, tx, user, rt.FamilyID); err != nil {
			return err
		}
		return s.sessions.MarkReplaced(ctx, tx, rt.ID, next)
	})
	if err != nil {
		return domain.Session{}, fmt.Errorf("refresh: %w", err)
	}
	if reused != "" {
		telemetry.LoggerFromContext(ctx).Warn("refresh token reused; login revoked", "family_id", reused)
		return domain.Session{}, errBadRefresh("reused refresh token, family %s revoked", reused)
	}
	return sess, nil
}

// Logout revokes the caller's access token and, if rawRefresh is the
// caller's, every refresh token of its login. Visitors have nothing to log
// out.
func (s *Service) Logout(ctx context.Context, rawRefresh string) error {
	a, _ := authctx.FromContext(ctx)
	if !a.Authenticated() {
		return ycerrors.Unauthorized("Authentication required")
	}
	if a.ActorType == authctx.ActorVisitor {
		return nil
	}
	access, verr := s.signer.Verify(a.Token)
	err := s.inTenant(ctx, a.TenantID, func(tx pgx.Tx) error {
		if verr == nil && access.UserID == a.ActorID {
			if err := s.sessions.RevokeAccess(ctx, tx, a.TenantID, access.TokenID, access.ExpiresAt); err != nil {
				return err
			}
		}
		if rawRefresh == "" {
			return nil
		}
		rt, err := s.sessions.GetRefreshForUpdate(ctx, tx, hashSecret(rawRefresh))
		if ycerrors.HasCode(err, ycerrors.CodeNotFound) || (err == nil && rt.UserID != a.ActorID) {
			return nil
		}
		if err != nil {
			return err
		}
		return s.sessions.RevokeFamily(ctx, tx, rt.FamilyID)
	})
	if err != nil {
		return fmt.Errorf("logout: %w", err)
	}
	return nil
}

// errBadClient is the only service-token failure a caller sees.
func errBadClient(internal string, args ...any) error {
	return ycerrors.Unauthorized("Invalid client credentials").WithInternal(internal, args...)
}

// IssueServiceToken checks a service client's credentials and signs a
// service token (ADR-021 rule 2).
func (s *Service) IssueServiceToken(_ context.Context, clientID, secret string) (domain.Session, error) {
	got := sha256.Sum256([]byte(secret))
	want, ok := s.clients[clientID]
	if subtle.ConstantTimeCompare(got[:], want[:]) != 1 || !ok {
		return domain.Session{}, errBadClient("client %q unknown or wrong secret", clientID)
	}
	return s.signer.Issue(domain.User{
		ID: clientID, TenantID: dbkit.PlatformTenantID, Role: domain.RoleService,
	}, s.cfg.ServiceTokenTTL)
}

func (s *Service) validateServiceToken(sess domain.Session) (domain.Session, error) {
	if sess.TenantID != dbkit.PlatformTenantID {
		return domain.Session{}, ycerrors.Unauthorized("Invalid or expired token").
			WithInternal("service token for tenant %s", sess.TenantID)
	}
	if _, ok := s.clients[sess.UserID]; !ok {
		return domain.Session{}, ycerrors.Unauthorized("Invalid or expired token").
			WithInternal("service client %s is no longer configured", sess.UserID)
	}
	return sess, nil
}

// validateTenant checks the tenant yc-gateway resolved; _platform is
// allowed.
func validateTenant(tenantID string) error {
	if tenantID == "" {
		return ycerrors.Validation().Field("tenant_id", "required", "Tenant is required").Build()
	}
	if err := eventbus.ValidateTenantID(tenantID); err != nil {
		return ycerrors.Validation().Field("tenant_id", "invalid", "Tenant ID is not valid").Wrap(err)
	}
	return nil
}

// callerActor is the event actor for a request that may be anonymous.
func callerActor(a authctx.AuthContext) eventbus.Actor {
	if a.Authenticated() {
		return eventbus.Actor{Type: string(a.ActorType), ID: a.ActorID}
	}
	return eventbus.Actor{Type: string(authctx.ActorService), ID: "yc-identity"}
}
