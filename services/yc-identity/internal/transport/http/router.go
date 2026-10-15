// Package http is yc-identity's HTTP transport. Routing, parameter parsing
// and request/response types are generated from api/openapi.yaml.
package http

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	api "github.com/slaghuis/yc-platform/services/yc-identity/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/repository"
)

// Identity is the service the handlers call.
type Identity interface {
	CreateVisitor(ctx context.Context, tenantID string) (domain.Session, error)
	Register(ctx context.Context, reg domain.Registration) (domain.Session, error)
	Login(ctx context.Context, email, password string) (domain.Session, error)
	Refresh(ctx context.Context, raw string) (domain.Session, error)
	Logout(ctx context.Context, rawRefresh string) error
	RequestPasswordReset(ctx context.Context, email, host string) error
	ConfirmPasswordReset(ctx context.Context, token, password string) error
	StartOAuth(ctx context.Context, provider, host, loginHint string) (string, error)
	CompleteOAuth(ctx context.Context, provider, code, state string) (domain.Session, error)
	ValidateToken(ctx context.Context, raw string) (domain.Session, error)
	IssueServiceToken(ctx context.Context, clientID, secret string) (domain.Session, error)
	Me(ctx context.Context) (domain.User, error)
	ListUsers(ctx context.Context, f repository.Filter, after *repository.Cursor, limit int) ([]domain.User, *repository.Cursor, error)
	GetUser(ctx context.Context, id string) (domain.User, error)
	Promote(ctx context.Context, id string, role domain.Role) (domain.User, error)
}

// NewRouter returns the service's routes on the standard httpkit chain.
// /internal/ routes are reached through yc-gateway's internal listener
// (ADR-021).
func NewRouter(svc Identity, build httpkit.BuildInfo, checks []httpkit.Check) http.Handler {
	r := httpkit.NewRouter(httpkit.RouterOptions{
		Build:  build,
		Checks: checks,
		Middleware: []func(http.Handler) http.Handler{
			authctx.Middleware, authctx.Audience, httpkit.LimitBody(httpkit.MaxBodyBytes), withHost,
		},
	})
	strict := api.NewStrictHandlerWithOptions(handlers{svc: svc}, nil, api.StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  httpkit.RequestError,
		ResponseErrorHandlerFunc: httpkit.ResponseError,
	})
	api.HandlerWithOptions(strict, api.ChiServerOptions{BaseRouter: r, ErrorHandlerFunc: httpkit.ParamError})
	return r
}

type handlers struct {
	svc Identity
}

type hostKey struct{}

// withHost records the host the client called: yc-gateway's
// X-Forwarded-Host, or the request's own Host when called directly.
func withHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Header.Get("X-Forwarded-Host")
		if host == "" {
			host = r.Host
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), hostKey{}, host)))
	})
}

var _ api.StrictServerInterface = handlers{}

// CreateVisitor issues a visitor token for the tenant yc-gateway resolved.
func (h handlers) CreateVisitor(ctx context.Context, _ api.CreateVisitorRequestObject) (api.CreateVisitorResponseObject, error) {
	a, _ := authctx.FromContext(ctx)
	sess, err := h.svc.CreateVisitor(ctx, a.TenantID)
	if err != nil {
		return nil, err
	}
	return api.CreateVisitor201JSONResponse(session(sess)), nil
}

func (h handlers) Register(ctx context.Context, req api.RegisterRequestObject) (api.RegisterResponseObject, error) {
	var displayName string
	if req.Body.DisplayName != nil {
		displayName = *req.Body.DisplayName
	}
	reg, err := domain.NewRegistration(string(req.Body.Email), req.Body.Password, displayName)
	if err != nil {
		return nil, err
	}
	sess, err := h.svc.Register(ctx, reg)
	if err != nil {
		return nil, err
	}
	return api.Register201JSONResponse(session(sess)), nil
}

func (h handlers) Login(ctx context.Context, req api.LoginRequestObject) (api.LoginResponseObject, error) {
	sess, err := h.svc.Login(ctx, req.Body.Email, req.Body.Password)
	if err != nil {
		return nil, err
	}
	return api.Login200JSONResponse(session(sess)), nil
}

func (h handlers) Refresh(ctx context.Context, req api.RefreshRequestObject) (api.RefreshResponseObject, error) {
	if req.Body.RefreshToken == "" {
		return nil, ycerrors.Validation().Field("refresh_token", "required", "Refresh token is required").Build()
	}
	sess, err := h.svc.Refresh(ctx, req.Body.RefreshToken)
	if err != nil {
		return nil, err
	}
	return api.Refresh200JSONResponse(session(sess)), nil
}

func (h handlers) Logout(ctx context.Context, req api.LogoutRequestObject) (api.LogoutResponseObject, error) {
	var refresh string
	if req.Body != nil && req.Body.RefreshToken != nil {
		refresh = *req.Body.RefreshToken
	}
	if err := h.svc.Logout(ctx, refresh); err != nil {
		return nil, err
	}
	return api.Logout204Response{}, nil
}

func (h handlers) RequestPasswordReset(ctx context.Context, req api.RequestPasswordResetRequestObject) (api.RequestPasswordResetResponseObject, error) {
	host, _ := ctx.Value(hostKey{}).(string)
	if err := h.svc.RequestPasswordReset(ctx, req.Body.Email, host); err != nil {
		return nil, err
	}
	return api.RequestPasswordReset202Response{}, nil
}

func (h handlers) ConfirmPasswordReset(ctx context.Context, req api.ConfirmPasswordResetRequestObject) (api.ConfirmPasswordResetResponseObject, error) {
	if err := h.svc.ConfirmPasswordReset(ctx, req.Body.Token, req.Body.Password); err != nil {
		return nil, err
	}
	return api.ConfirmPasswordReset204Response{}, nil
}

func (h handlers) StartOAuth(ctx context.Context, req api.StartOAuthRequestObject) (api.StartOAuthResponseObject, error) {
	var hint string
	if req.Body != nil && req.Body.LoginHint != nil {
		hint = *req.Body.LoginHint
	}
	host, _ := ctx.Value(hostKey{}).(string)
	u, err := h.svc.StartOAuth(ctx, string(req.Provider), host, hint)
	if err != nil {
		return nil, err
	}
	return api.StartOAuth200JSONResponse{AuthorizationURL: u}, nil
}

func (h handlers) CompleteOAuth(ctx context.Context, req api.CompleteOAuthRequestObject) (api.CompleteOAuthResponseObject, error) {
	sess, err := h.svc.CompleteOAuth(ctx, string(req.Provider), req.Body.Code, req.Body.State)
	if err != nil {
		return nil, err
	}
	return api.CompleteOAuth200JSONResponse(session(sess)), nil
}

func (h handlers) IssueServiceToken(ctx context.Context, req api.IssueServiceTokenRequestObject) (api.IssueServiceTokenResponseObject, error) {
	sess, err := h.svc.IssueServiceToken(ctx, req.Body.ClientID, req.Body.ClientSecret)
	if err != nil {
		return nil, err
	}
	return api.IssueServiceToken200JSONResponse{
		AccessToken: sess.Token, TokenType: "Bearer", ExpiresAt: sess.ExpiresAt.UTC(),
	}, nil
}

func (h handlers) GetMe(ctx context.Context, _ api.GetMeRequestObject) (api.GetMeResponseObject, error) {
	u, err := h.svc.Me(ctx)
	if err != nil {
		return nil, err
	}
	return api.GetMe200JSONResponse(user(u)), nil
}

func (h handlers) ListUsers(ctx context.Context, req api.ListUsersRequestObject) (api.ListUsersResponseObject, error) {
	p := req.Params
	page, err := httpkit.ParsePage(p.PageSize, p.Cursor)
	if err != nil {
		return nil, err
	}
	var after *repository.Cursor
	if page.Cursor != "" {
		after = &repository.Cursor{}
		if err := httpkit.DecodeCursor(page.Cursor, after); err != nil {
			return nil, err
		}
	}
	f := repository.Filter{}
	if p.TenantID != nil {
		f.TenantID = *p.TenantID
	}
	if p.Email != nil {
		norm, _ := domain.NormaliseEmail(*p.Email)
		f.Email = norm
	}
	if p.Role != nil {
		f.Role = domain.Role(*p.Role)
		if !f.Role.Valid() {
			return nil, ycerrors.Validation().Field("role", "invalid", "Unknown role").Build()
		}
	}

	users, next, err := h.svc.ListUsers(ctx, f, after, page.Size)
	if err != nil {
		return nil, err
	}
	out := api.ListUsers200JSONResponse{
		Items: make([]api.User, len(users)),
		Page:  httpkit.PageInfo{Size: page.Size, Cursor: page.Cursor},
	}
	for i, u := range users {
		out.Items[i] = user(u)
	}
	if next != nil {
		out.Page.NextCursor = httpkit.EncodeCursor(next)
	}
	return out, nil
}

func (h handlers) GetUser(ctx context.Context, req api.GetUserRequestObject) (api.GetUserResponseObject, error) {
	u, err := h.svc.GetUser(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.GetUser200JSONResponse(user(u)), nil
}

func (h handlers) PromoteUser(ctx context.Context, req api.PromoteUserRequestObject) (api.PromoteUserResponseObject, error) {
	u, err := h.svc.Promote(ctx, req.ID.String(), domain.Role(req.Body.Role))
	if err != nil {
		return nil, err
	}
	return api.PromoteUser200JSONResponse(user(u)), nil
}

func (h handlers) ValidateToken(ctx context.Context, req api.ValidateTokenRequestObject) (api.ValidateTokenResponseObject, error) {
	if req.Body.Token == "" {
		return nil, ycerrors.Validation().Field("token", "required", "Token is required").Build()
	}
	sess, err := h.svc.ValidateToken(ctx, req.Body.Token)
	if err != nil {
		return nil, err
	}
	return api.ValidateToken200JSONResponse{
		TenantID:  sess.TenantID,
		ActorID:   sess.UserID,
		ActorType: api.ActorType(sess.Role.ActorType()),
		Jti:       parseUUID(sess.TokenID),
		ExpiresAt: sess.ExpiresAt.UTC(),
	}, nil
}

func session(s domain.Session) api.Session {
	out := api.Session{
		AccessToken: s.Token,
		TokenType:   "Bearer",
		ExpiresAt:   s.ExpiresAt.UTC(),
		UserID:      parseUUID(s.UserID),
		TenantID:    s.TenantID,
		Role:        api.Role(s.Role),
	}
	if s.RefreshToken != "" {
		exp := s.RefreshExpiresAt.UTC()
		out.RefreshToken, out.RefreshExpiresAt = &s.RefreshToken, &exp
	}
	return out
}

func user(u domain.User) api.User {
	out := api.User{
		UserID:    parseUUID(u.ID),
		TenantID:  u.TenantID,
		Role:      api.Role(u.Role),
		Status:    api.UserStatus(u.Status),
		CreatedAt: u.CreatedAt.UTC(),
	}
	if u.Email != "" {
		out.Email = &u.Email
	}
	if u.DisplayName != "" {
		out.DisplayName = &u.DisplayName
	}
	return out
}

// parseUUID converts IDs the database generated; they are always UUIDs.
func parseUUID(s string) uuid.UUID {
	id, _ := uuid.Parse(s)
	return id
}
