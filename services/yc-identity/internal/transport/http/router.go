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
	Suspend(ctx context.Context, id, reason string) (domain.User, error)
	Reactivate(ctx context.Context, id string) (domain.User, error)
	Demote(ctx context.Context, id string) (domain.User, error)
	DeleteMe(ctx context.Context) error
	DeleteUser(ctx context.Context, id string) error
	GetMyBilling(ctx context.Context) (domain.BillingAddress, error)
	PutMyBilling(ctx context.Context, in domain.BillingAddress) (domain.BillingAddress, error)
	DeleteMyBilling(ctx context.Context) error
	UserBilling(ctx context.Context, userID string) (domain.BillingAddress, error)
	ListMyAddresses(ctx context.Context, includeDeleted bool) ([]domain.DeliveryAddress, error)
	GetMyAddress(ctx context.Context, id string) (domain.DeliveryAddress, error)
	CreateMyAddress(ctx context.Context, in domain.DeliveryAddressInput) (domain.DeliveryAddress, error)
	PutMyAddress(ctx context.Context, id string, in domain.DeliveryAddressInput) (domain.DeliveryAddress, error)
	DeleteMyAddress(ctx context.Context, id string) error
	ListUserAddresses(ctx context.Context, userID string, includeDeleted bool) ([]domain.DeliveryAddress, error)
	GetUserAddress(ctx context.Context, userID, id string) (domain.DeliveryAddress, error)
	UserAddress(ctx context.Context, userID, id string) (domain.DeliveryAddress, error)
	CreateUserAddress(ctx context.Context, userID string, in domain.DeliveryAddressInput) (domain.DeliveryAddress, error)
	CreateInvitation(ctx context.Context, in domain.InviteInput) (domain.Invitation, error)
	ListInvitations(ctx context.Context, f repository.InvitationFilter, after *repository.InvitationCursor, limit int) ([]domain.Invitation, *repository.InvitationCursor, error)
	ResendInvitation(ctx context.Context, id string) (domain.Invitation, error)
	RevokeInvitation(ctx context.Context, id string) error
	AcceptInvitation(ctx context.Context, in domain.AcceptInviteInput) (domain.Session, error)
}

// Routes are the gateway routes yc-identity registers (ADR-020 rule 7).
// Accept stays on the static chain; gateway adds it separately.
func Routes() []httpkit.RegisteredRoute {
	sysadmin := []string{string(authctx.ActorSystemAdmin)}
	return []httpkit.RegisteredRoute{
		{Prefix: "/sysadmin/invitations", Roles: sysadmin},
		{Prefix: "/internal/users", Methods: []string{http.MethodGet, http.MethodPost}},
	}
}

// NewRouter returns the service's routes on the standard httpkit chain.
// POSTs carrying an Idempotency-Key are replayed from idem.
// /internal/ routes are reached through yc-gateway's internal listener
// (ADR-021).
func NewRouter(svc Identity, idem httpkit.IdempotencyStore, build httpkit.BuildInfo, checks []httpkit.Check) http.Handler {
	mw := []func(http.Handler) http.Handler{
		authctx.Middleware, authctx.Audience, httpkit.LimitBody(httpkit.MaxBodyBytes), withHost,
	}
	if idem != nil {
		mw = append(mw, httpkit.Idempotency(httpkit.IdempotencyOptions{Store: idem}))
	}
	r := httpkit.NewRouter(httpkit.RouterOptions{
		Build: build, Checks: checks, Middleware: mw,
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

func (h handlers) SuspendUser(ctx context.Context, req api.SuspendUserRequestObject) (api.SuspendUserResponseObject, error) {
	var reason string
	if req.Body != nil && req.Body.Reason != nil {
		reason = *req.Body.Reason
	}
	u, err := h.svc.Suspend(ctx, req.ID.String(), reason)
	if err != nil {
		return nil, err
	}
	return api.SuspendUser200JSONResponse(user(u)), nil
}

func (h handlers) ReactivateUser(ctx context.Context, req api.ReactivateUserRequestObject) (api.ReactivateUserResponseObject, error) {
	u, err := h.svc.Reactivate(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.ReactivateUser200JSONResponse(user(u)), nil
}

func (h handlers) DemoteUser(ctx context.Context, req api.DemoteUserRequestObject) (api.DemoteUserResponseObject, error) {
	u, err := h.svc.Demote(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.DemoteUser200JSONResponse(user(u)), nil
}

func (h handlers) CreateInvitation(ctx context.Context, req api.CreateInvitationRequestObject) (api.CreateInvitationResponseObject, error) {
	var displayName, acceptHost string
	if req.Body.DisplayName != nil {
		displayName = *req.Body.DisplayName
	}
	if req.Body.AcceptHost != nil {
		acceptHost = *req.Body.AcceptHost
	}
	in, err := domain.NewInviteInput(req.Body.TenantID, string(req.Body.Email), string(req.Body.Role), displayName, acceptHost)
	if err != nil {
		return nil, err
	}
	inv, err := h.svc.CreateInvitation(ctx, in)
	if err != nil {
		return nil, err
	}
	return api.CreateInvitation201JSONResponse(invitation(inv)), nil
}

func (h handlers) ListInvitations(ctx context.Context, req api.ListInvitationsRequestObject) (api.ListInvitationsResponseObject, error) {
	p := req.Params
	page, err := httpkit.ParsePage(p.PageSize, p.Cursor)
	if err != nil {
		return nil, err
	}
	var after *repository.InvitationCursor
	if page.Cursor != "" {
		after = &repository.InvitationCursor{}
		if err := httpkit.DecodeCursor(page.Cursor, after); err != nil {
			return nil, err
		}
	}
	f := repository.InvitationFilter{}
	if p.TenantID != nil {
		f.TenantID = *p.TenantID
	}
	if p.Status != nil {
		f.Status = domain.InvitationStatus(*p.Status)
	}
	invs, next, err := h.svc.ListInvitations(ctx, f, after, page.Size)
	if err != nil {
		return nil, err
	}
	out := api.ListInvitations200JSONResponse{
		Items: make([]api.Invitation, len(invs)),
		Page:  httpkit.PageInfo{Size: page.Size, Cursor: page.Cursor},
	}
	for i, inv := range invs {
		out.Items[i] = invitation(inv)
	}
	if next != nil {
		out.Page.NextCursor = httpkit.EncodeCursor(next)
	}
	return out, nil
}

func (h handlers) ResendInvitation(ctx context.Context, req api.ResendInvitationRequestObject) (api.ResendInvitationResponseObject, error) {
	inv, err := h.svc.ResendInvitation(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.ResendInvitation200JSONResponse(invitation(inv)), nil
}

func (h handlers) RevokeInvitation(ctx context.Context, req api.RevokeInvitationRequestObject) (api.RevokeInvitationResponseObject, error) {
	if err := h.svc.RevokeInvitation(ctx, req.ID.String()); err != nil {
		return nil, err
	}
	return api.RevokeInvitation204Response{}, nil
}

func (h handlers) AcceptInvitation(ctx context.Context, req api.AcceptInvitationRequestObject) (api.AcceptInvitationResponseObject, error) {
	var displayName string
	if req.Body.DisplayName != nil {
		displayName = *req.Body.DisplayName
	}
	in, err := domain.NewAcceptInviteInput(req.Body.Token, req.Body.Password, displayName)
	if err != nil {
		return nil, err
	}
	sess, err := h.svc.AcceptInvitation(ctx, in)
	if err != nil {
		return nil, err
	}
	return api.AcceptInvitation200JSONResponse(session(sess)), nil
}

func (h handlers) GetMyBilling(ctx context.Context, _ api.GetMyBillingRequestObject) (api.GetMyBillingResponseObject, error) {
	b, err := h.svc.GetMyBilling(ctx)
	if err != nil {
		return nil, err
	}
	return api.GetMyBilling200JSONResponse(billing(b)), nil
}

func (h handlers) PutMyBilling(ctx context.Context, req api.PutMyBillingRequestObject) (api.PutMyBillingResponseObject, error) {
	b, err := h.svc.PutMyBilling(ctx, billingIn(*req.Body))
	if err != nil {
		return nil, err
	}
	return api.PutMyBilling200JSONResponse(billing(b)), nil
}

func (h handlers) DeleteMyBilling(ctx context.Context, _ api.DeleteMyBillingRequestObject) (api.DeleteMyBillingResponseObject, error) {
	if err := h.svc.DeleteMyBilling(ctx); err != nil {
		return nil, err
	}
	return api.DeleteMyBilling204Response{}, nil
}

func (h handlers) GetUserBilling(ctx context.Context, req api.GetUserBillingRequestObject) (api.GetUserBillingResponseObject, error) {
	b, err := h.svc.UserBilling(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.GetUserBilling200JSONResponse(billing(b)), nil
}

func (h handlers) ListMyAddresses(ctx context.Context, req api.ListMyAddressesRequestObject) (api.ListMyAddressesResponseObject, error) {
	include := req.Params.IncludeDeleted != nil && *req.Params.IncludeDeleted
	items, err := h.svc.ListMyAddresses(ctx, include)
	if err != nil {
		return nil, err
	}
	return api.ListMyAddresses200JSONResponse{Items: deliveryAddresses(items)}, nil
}

func (h handlers) CreateMyAddress(ctx context.Context, req api.CreateMyAddressRequestObject) (api.CreateMyAddressResponseObject, error) {
	a, err := h.svc.CreateMyAddress(ctx, deliveryAddressIn(*req.Body))
	if err != nil {
		return nil, err
	}
	return api.CreateMyAddress201JSONResponse(deliveryAddress(a)), nil
}

func (h handlers) GetMyAddress(ctx context.Context, req api.GetMyAddressRequestObject) (api.GetMyAddressResponseObject, error) {
	a, err := h.svc.GetMyAddress(ctx, req.ID.String())
	if err != nil {
		return nil, err
	}
	return api.GetMyAddress200JSONResponse(deliveryAddress(a)), nil
}

func (h handlers) PutMyAddress(ctx context.Context, req api.PutMyAddressRequestObject) (api.PutMyAddressResponseObject, error) {
	a, err := h.svc.PutMyAddress(ctx, req.ID.String(), deliveryAddressIn(*req.Body))
	if err != nil {
		return nil, err
	}
	return api.PutMyAddress200JSONResponse(deliveryAddress(a)), nil
}

func (h handlers) DeleteMyAddress(ctx context.Context, req api.DeleteMyAddressRequestObject) (api.DeleteMyAddressResponseObject, error) {
	if err := h.svc.DeleteMyAddress(ctx, req.ID.String()); err != nil {
		return nil, err
	}
	return api.DeleteMyAddress204Response{}, nil
}

func (h handlers) ListUserAddresses(ctx context.Context, req api.ListUserAddressesRequestObject) (api.ListUserAddressesResponseObject, error) {
	include := req.Params.IncludeDeleted != nil && *req.Params.IncludeDeleted
	items, err := h.svc.ListUserAddresses(ctx, req.ID.String(), include)
	if err != nil {
		return nil, err
	}
	return api.ListUserAddresses200JSONResponse{Items: deliveryAddresses(items)}, nil
}

func (h handlers) GetUserAddress(ctx context.Context, req api.GetUserAddressRequestObject) (api.GetUserAddressResponseObject, error) {
	a, err := h.svc.GetUserAddress(ctx, req.ID.String(), req.AddressID.String())
	if err != nil {
		return nil, err
	}
	return api.GetUserAddress200JSONResponse(deliveryAddress(a)), nil
}

func (h handlers) GetInternalUserAddress(ctx context.Context, req api.GetInternalUserAddressRequestObject) (api.GetInternalUserAddressResponseObject, error) {
	a, err := h.svc.UserAddress(ctx, req.ID.String(), req.AddressID.String())
	if err != nil {
		return nil, err
	}
	return api.GetInternalUserAddress200JSONResponse(deliveryAddress(a)), nil
}

func (h handlers) CreateUserAddress(ctx context.Context, req api.CreateUserAddressRequestObject) (api.CreateUserAddressResponseObject, error) {
	a, err := h.svc.CreateUserAddress(ctx, req.ID.String(), deliveryAddressIn(*req.Body))
	if err != nil {
		return nil, err
	}
	return api.CreateUserAddress201JSONResponse(deliveryAddress(a)), nil
}

func deliveryAddresses(items []domain.DeliveryAddress) []api.DeliveryAddress {
	out := make([]api.DeliveryAddress, len(items))
	for i, a := range items {
		out[i] = deliveryAddress(a)
	}
	return out
}

func deliveryAddress(a domain.DeliveryAddress) api.DeliveryAddress {
	out := api.DeliveryAddress{
		ID: mustUUID(a.ID), Address: a.Address, IsDefault: a.IsDefault,
		CreatedAt: a.CreatedAt.UTC(), UpdatedAt: a.UpdatedAt.UTC(),
	}
	if a.Label != "" {
		out.Label = &a.Label
	}
	if a.DeletedAt != nil {
		t := a.DeletedAt.UTC()
		out.DeletedAt = &t
	}
	if a.ReplacedByID != "" {
		if id, err := uuid.Parse(a.ReplacedByID); err == nil {
			out.ReplacedByID = &id
		}
	}
	return out
}

func deliveryAddressIn(b api.DeliveryAddressInput) domain.DeliveryAddressInput {
	in := domain.DeliveryAddressInput{Address: b.Address}
	if b.Label != nil {
		in.Label = *b.Label
	}
	if b.IsDefault != nil {
		in.IsDefault = *b.IsDefault
	}
	return in
}

func mustUUID(s string) uuid.UUID {
	id, _ := uuid.Parse(s)
	return id
}

func billing(b domain.BillingAddress) api.BillingAddress {
	out := api.BillingAddress{
		StreetAndNumber: b.StreetAndNumber, TownCity: b.TownCity, PostalCode: b.PostalCode,
	}
	if b.CompanyName != "" {
		out.CompanyName = &b.CompanyName
	}
	if b.VATNumber != "" {
		out.VatNumber = &b.VATNumber
	}
	if b.Suburb != "" {
		out.Suburb = &b.Suburb
	}
	if b.CountryCode != "" {
		out.CountryCode = &b.CountryCode
	}
	return out
}

func billingIn(b api.BillingAddress) domain.BillingAddress {
	out := domain.BillingAddress{
		StreetAndNumber: b.StreetAndNumber, TownCity: b.TownCity, PostalCode: b.PostalCode,
	}
	if b.CompanyName != nil {
		out.CompanyName = *b.CompanyName
	}
	if b.VatNumber != nil {
		out.VATNumber = *b.VatNumber
	}
	if b.Suburb != nil {
		out.Suburb = *b.Suburb
	}
	if b.CountryCode != nil {
		out.CountryCode = *b.CountryCode
	}
	return out
}

func (h handlers) DeleteMe(ctx context.Context, _ api.DeleteMeRequestObject) (api.DeleteMeResponseObject, error) {
	if err := h.svc.DeleteMe(ctx); err != nil {
		return nil, err
	}
	return api.DeleteMe204Response{}, nil
}

func (h handlers) DeleteUser(ctx context.Context, req api.DeleteUserRequestObject) (api.DeleteUserResponseObject, error) {
	if err := h.svc.DeleteUser(ctx, req.ID.String()); err != nil {
		return nil, err
	}
	return api.DeleteUser204Response{}, nil
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
	if u.PhoneE164 != "" {
		out.PhoneE164 = &u.PhoneE164
	}
	if u.ProfileImage != "" {
		out.ProfileImageURL = &u.ProfileImage
	}
	return out
}

func invitation(inv domain.Invitation) api.Invitation {
	out := api.Invitation{
		InvitationID: parseUUID(inv.ID),
		TenantID:     inv.TenantID,
		Email:        inv.Email,
		Role:         api.InvitationRole(inv.Role),
		Status:       api.InvitationStatus(inv.Status),
		ExpiresAt:    inv.ExpiresAt.UTC(),
		CreatedAt:    inv.CreatedAt.UTC(),
	}
	if inv.DisplayName != "" {
		out.DisplayName = &inv.DisplayName
	}
	if inv.InvitedBy != "" {
		id := parseUUID(inv.InvitedBy)
		out.InvitedBy = &id
	}
	if inv.AcceptedUserID != "" {
		id := parseUUID(inv.AcceptedUserID)
		out.AcceptedUserID = &id
	}
	return out
}

// parseUUID converts IDs the database generated; they are always UUIDs.
func parseUUID(s string) uuid.UUID {
	id, _ := uuid.Parse(s)
	return id
}
