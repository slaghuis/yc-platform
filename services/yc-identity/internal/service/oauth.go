package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/oauth"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/repository"
)

func (s *Service) provider(name string) (OAuthProvider, error) {
	p, ok := s.cfg.OAuth[name]
	if !ok {
		return OAuthProvider{}, ycerrors.NotFound("OAuth provider", name)
	}
	return p, nil
}

// StartOAuth stores a pending authorisation for the calling visitor and
// returns the provider URL to send the browser to. host is the shop host
// the request came in on, for the callback page.
func (s *Service) StartOAuth(ctx context.Context, provider, host, loginHint string) (string, error) {
	p, err := s.provider(provider)
	if err != nil {
		return "", err
	}
	a, _ := authctx.FromContext(ctx)
	if err := validateShopTenant(a.TenantID); err != nil {
		return "", err
	}
	if a.Authenticated() && a.ActorType != authctx.ActorVisitor {
		return "", ycerrors.Conflict("Already signed in")
	}
	redirect := p.RedirectURL
	if strings.Contains(redirect, "{host}") {
		if host == "" {
			return "", ycerrors.Validation().Message("The request has no host to return to").Build()
		}
		redirect = strings.ReplaceAll(redirect, "{host}", host)
	}
	state, hash := newSecret()
	verifier := oauth.NewVerifier()
	var visitor string
	if a.ActorType == authctx.ActorVisitor {
		visitor = a.ActorID
	}
	err = dbkit.WithTenantTx(ctx, s.db, a.TenantID, func(tx pgx.Tx) error {
		if err := s.oauth.DeleteExpiredStates(ctx, tx, s.now()); err != nil {
			return err
		}
		return s.oauth.InsertState(ctx, tx, repository.OAuthState{
			TenantID: a.TenantID, Provider: provider, CodeVerifier: verifier,
			RedirectURL: redirect, VisitorID: visitor, ExpiresAt: s.now().Add(s.cfg.OAuthStateTTL),
		}, hash)
	})
	if err != nil {
		return "", fmt.Errorf("start oauth: %w", err)
	}
	return p.Provider.AuthCodeURL(state, verifier, redirect, loginHint), nil
}

func errOAuthState(internal string, args ...any) error {
	return ycerrors.Unauthorized("Sign-in expired or was already used; please try again").WithInternal(internal, args...)
}

// oauthOutcome is what CompleteOAuth did to find the user.
type oauthOutcome int

const (
	oauthLoggedIn   oauthOutcome = iota // an existing link
	oauthLinked                         // linked to an existing account by verified email
	oauthRegistered                     // a new customer
)

// CompleteOAuth exchanges the provider's code and signs the account in,
// linking or registering it (docs/services/yc-identity.md, OAuth).
func (s *Service) CompleteOAuth(ctx context.Context, provider, code, state string) (domain.Session, error) {
	p, err := s.provider(provider)
	if err != nil {
		return domain.Session{}, err
	}
	a, _ := authctx.FromContext(ctx)
	if err := validateShopTenant(a.TenantID); err != nil {
		return domain.Session{}, err
	}

	var st repository.OAuthState
	err = dbkit.WithTenantTx(ctx, s.db, a.TenantID, func(tx pgx.Tx) error {
		var err error
		st, err = s.oauth.TakeState(ctx, tx, hashSecret(state))
		return err
	})
	if ycerrors.HasCode(err, ycerrors.CodeNotFound) {
		return domain.Session{}, errOAuthState("unknown state")
	}
	if err != nil {
		return domain.Session{}, err
	}
	if st.Provider != provider || !s.now().Before(st.ExpiresAt) {
		return domain.Session{}, errOAuthState("state for %s, expires %s", st.Provider, st.ExpiresAt)
	}

	profile, err := p.Provider.Exchange(ctx, code, st.CodeVerifier, st.RedirectURL)
	if errors.Is(err, oauth.ErrExchange) {
		return domain.Session{}, ycerrors.Unauthorized("Sign-in with " + provider + " failed").Wrap(err)
	}
	if err != nil {
		return domain.Session{}, ycerrors.UpstreamFailure("oauth " + provider).Wrap(err)
	}

	var (
		sess    domain.Session
		outcome oauthOutcome
		email   string
	)
	err = dbkit.WithTenantTx(ctx, s.db, a.TenantID, func(tx pgx.Tx) error {
		user, out, err := s.resolveOAuthUser(ctx, tx, a.TenantID, provider, profile, st.VisitorID)
		if err != nil {
			return err
		}
		outcome, email = out, user.Email
		before := user
		user.MergeOAuthProfile(profile)
		if user.PhoneE164 != before.PhoneE164 || user.ProfileImage != before.ProfileImage {
			if err := s.users.ApplyOAuthProfile(ctx, tx, user); err != nil {
				return err
			}
		}
		if sess, _, err = s.issueSession(ctx, tx, user, ""); err != nil {
			return err
		}
		return s.publishOAuth(ctx, tx, user, sess, provider, outcome, st.VisitorID)
	})
	if err != nil {
		return domain.Session{}, fmt.Errorf("complete oauth: %w", err)
	}
	if outcome == oauthRegistered {
		s.claimSupport(ctx, a.TenantID, sess.UserID, email)
	}
	return sess, nil
}

func (s *Service) resolveOAuthUser(ctx context.Context, tx pgx.Tx, tenantID, provider string, p domain.OAuthProfile, visitorID string) (domain.User, oauthOutcome, error) {
	userID, err := s.oauth.LinkedUser(ctx, tx, tenantID, provider, p.Subject)
	if err == nil {
		user, err := s.users.Get(ctx, tx, userID)
		if err != nil {
			return domain.User{}, 0, err
		}
		if user.Status != domain.StatusActive {
			return domain.User{}, 0, ycerrors.Unauthorized("Sign-in with "+provider+" failed").
				WithInternal("user %s is %s", user.ID, user.Status)
		}
		return user, oauthLoggedIn, nil
	}
	if !ycerrors.HasCode(err, ycerrors.CodeNotFound) {
		return domain.User{}, 0, err
	}

	if p.Email == "" {
		return domain.User{}, 0, ycerrors.Validation().
			Message("Your " + provider + " account did not share an email address").Build()
	}
	existing, err := s.users.GetByEmail(ctx, tx, tenantID, p.Email)
	switch {
	case err == nil:
		if !p.EmailVerified {
			return domain.User{}, 0, ycerrors.Conflict("An account with this email already exists; sign in with your password").
				WithInternal("%s did not verify %s", provider, p.Email)
		}
		if existing.Status != domain.StatusActive {
			return domain.User{}, 0, ycerrors.Unauthorized("Sign-in with "+provider+" failed").
				WithInternal("user %s is %s", existing.ID, existing.Status)
		}
		if err := s.oauth.Link(ctx, tx, existing, provider, p); err != nil {
			return domain.User{}, 0, err
		}
		return existing, oauthLinked, nil
	case !ycerrors.HasCode(err, ycerrors.CodeNotFound):
		return domain.User{}, 0, err
	}

	var user domain.User
	if visitorID != "" {
		user, err = s.users.RegisterOAuth(ctx, tx, visitorID, p.Email, p.Name)
		if err != nil && !ycerrors.HasCode(err, ycerrors.CodeNotFound) {
			return domain.User{}, 0, err
		}
	}
	if user.ID == "" {
		if user, err = s.users.CreateOAuth(ctx, tx, tenantID, p.Email, p.Name); err != nil {
			return domain.User{}, 0, err
		}
	}
	if err := s.oauth.Link(ctx, tx, user, provider, p); err != nil {
		return domain.User{}, 0, err
	}
	return user, oauthRegistered, nil
}

// publishOAuth publishes registered and oauth_linked for new links, and
// logged_in whenever the visitor that started the flow is not the user
// itself, so yc-cart merges its cart.
func (s *Service) publishOAuth(ctx context.Context, tx pgx.Tx, user domain.User, sess domain.Session, provider string, outcome oauthOutcome, visitorID string) error {
	actor := eventbus.Actor{Type: string(user.Role.ActorType()), ID: user.ID, AccessTokenJTI: sess.TokenID}
	now := s.now().UTC()
	if outcome == oauthRegistered {
		var displayName *string
		if user.DisplayName != "" {
			displayName = &user.DisplayName
		}
		err := s.bus.Publish(ctx, tx, eventbus.Event{
			EventType: events.TypeIdentityUserRegisteredV1,
			TenantID:  user.TenantID,
			Actor:     actor,
			Payload: events.IdentityUserRegisteredV1{
				UserID: user.ID, Email: user.Email, DisplayName: displayName,
				Role: events.IdentityUserRegisteredV1RoleCustomer, Method: provider, RegisteredAt: now,
			},
		})
		if err != nil {
			return err
		}
	}
	if outcome != oauthLoggedIn {
		err := s.bus.Publish(ctx, tx, eventbus.Event{
			EventType: events.TypeIdentityOauthLinkedV1,
			TenantID:  user.TenantID,
			Actor:     actor,
			Payload:   events.IdentityOauthLinkedV1{UserID: user.ID, Provider: provider, LinkedAt: now},
		})
		if err != nil {
			return err
		}
	}
	if outcome == oauthRegistered && (visitorID == "" || visitorID == user.ID) {
		return nil
	}
	return s.publishLoggedIn(ctx, tx, user, sess, provider, visitorID)
}
