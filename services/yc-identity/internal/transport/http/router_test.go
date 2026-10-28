package http_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	api "github.com/slaghuis/yc-platform/services/yc-identity/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/repository"
	transport "github.com/slaghuis/yc-platform/services/yc-identity/internal/transport/http"
)

const (
	userID = "0192f3a4-5b6c-7d8e-9f01-23456789abcd"
	jti    = "0192f3a4-5b6c-7d8e-9f01-000000000001"
)

type fakeIdentity struct {
	tenant   string
	reg      domain.Registration
	filter   repository.Filter
	after    *repository.Cursor
	limit    int
	promoted domain.Role
	refresh  string
	email    string
	reset    string
	oauth    string
}

// internal marks req as an /internal/ call yc-gateway forwarded.
func internal(req *http.Request) *http.Request {
	req.Header.Set(authctx.HeaderCallerService, "yc-gateway")
	return req
}

func (f *fakeIdentity) CreateVisitor(_ context.Context, tenantID string) (domain.Session, error) {
	f.tenant = tenantID
	if tenantID == "" {
		return domain.Session{}, ycerrors.Validation().Field("tenant_id", "required", "Tenant is required").Build()
	}
	return domain.Session{Token: "tok", UserID: userID, TenantID: tenantID, Role: domain.RoleVisitor, ExpiresAt: time.Unix(1700000000, 0)}, nil
}

func (f *fakeIdentity) Register(ctx context.Context, reg domain.Registration) (domain.Session, error) {
	f.reg = reg
	a, _ := authctx.FromContext(ctx)
	return domain.Session{Token: "cust", UserID: userID, TenantID: a.TenantID, Role: domain.RoleCustomer}, nil
}

func (f *fakeIdentity) Login(_ context.Context, email, pw string) (domain.Session, error) {
	if email != "a@b.co" || pw != "secret-password" {
		return domain.Session{}, ycerrors.Unauthorized("Invalid email or password")
	}
	return domain.Session{Token: "cust", UserID: userID, TenantID: "shop-a", Role: domain.RoleCustomer}, nil
}

func (f *fakeIdentity) Refresh(_ context.Context, raw string) (domain.Session, error) {
	if raw != "r1" {
		return domain.Session{}, ycerrors.Unauthorized("Invalid or expired refresh token")
	}
	return domain.Session{
		Token: "cust", UserID: userID, TenantID: "shop-a", Role: domain.RoleCustomer,
		RefreshToken: "r2", RefreshExpiresAt: time.Unix(1700000000, 0),
	}, nil
}

func (f *fakeIdentity) Logout(_ context.Context, raw string) error {
	f.refresh = raw
	return nil
}

func (f *fakeIdentity) RequestPasswordReset(_ context.Context, email, _ string) error {
	f.email = email
	return nil
}

func (f *fakeIdentity) ConfirmPasswordReset(_ context.Context, token, pw string) error {
	f.reset = token + "/" + pw
	return nil
}

func (f *fakeIdentity) StartOAuth(_ context.Context, provider, host, hint string) (string, error) {
	f.oauth = provider + " " + host + " " + hint
	return "https://provider.test/auth", nil
}

func (f *fakeIdentity) CompleteOAuth(_ context.Context, provider, code, state string) (domain.Session, error) {
	f.oauth = provider + " " + code + " " + state
	return domain.Session{Token: "cust", UserID: userID, TenantID: "shop-a", Role: domain.RoleCustomer}, nil
}

func (f *fakeIdentity) IssueServiceToken(_ context.Context, id, secret string) (domain.Session, error) {
	if id != "yc-cart" || secret != "s" {
		return domain.Session{}, ycerrors.Unauthorized("Invalid client credentials")
	}
	return domain.Session{Token: "svc", UserID: id, TenantID: "_platform", Role: domain.RoleService, ExpiresAt: time.Unix(1700000000, 0)}, nil
}

func (f *fakeIdentity) ValidateToken(_ context.Context, raw string) (domain.Session, error) {
	if raw == "svc" {
		return domain.Session{TokenID: jti, UserID: "yc-cart", TenantID: "_platform", Role: domain.RoleService}, nil
	}
	if raw != "good" {
		return domain.Session{}, ycerrors.Unauthorized("Invalid or expired token")
	}
	return domain.Session{TokenID: jti, UserID: userID, TenantID: "shop-a", Role: domain.RoleVisitor}, nil
}

func (f *fakeIdentity) Me(ctx context.Context) (domain.User, error) {
	a, _ := authctx.FromContext(ctx)
	if !a.Authenticated() {
		return domain.User{}, ycerrors.Unauthorized("Authentication required")
	}
	return domain.User{ID: a.ActorID, TenantID: a.TenantID, Role: domain.Role(a.ActorType), Status: domain.StatusActive}, nil
}

func (f *fakeIdentity) ListUsers(_ context.Context, flt repository.Filter, after *repository.Cursor, limit int) ([]domain.User, *repository.Cursor, error) {
	f.filter, f.after, f.limit = flt, after, limit
	u := domain.User{ID: userID, TenantID: "shop-a", Email: "a@b.co", Role: domain.RoleCustomer, Status: domain.StatusActive}
	return []domain.User{u}, &repository.Cursor{ID: userID}, nil
}

func (f *fakeIdentity) GetUser(_ context.Context, id string) (domain.User, error) {
	return domain.User{ID: id, TenantID: "shop-a", Role: domain.RoleCustomer, Status: domain.StatusActive}, nil
}

func (f *fakeIdentity) Promote(_ context.Context, id string, role domain.Role) (domain.User, error) {
	f.promoted = role
	return domain.User{ID: id, TenantID: "shop-a", Role: role, Status: domain.StatusActive}, nil
}

func (f *fakeIdentity) Suspend(_ context.Context, id, _ string) (domain.User, error) {
	return domain.User{ID: id, TenantID: "shop-a", Role: domain.RoleCustomer, Status: domain.StatusSuspended}, nil
}

func (f *fakeIdentity) Reactivate(_ context.Context, id string) (domain.User, error) {
	return domain.User{ID: id, TenantID: "shop-a", Role: domain.RoleCustomer, Status: domain.StatusActive}, nil
}

func (f *fakeIdentity) Demote(_ context.Context, id string) (domain.User, error) {
	return domain.User{ID: id, TenantID: "shop-a", Role: domain.RoleCustomer, Status: domain.StatusActive}, nil
}

func (*fakeIdentity) DeleteMe(context.Context) error { return nil }

func (*fakeIdentity) DeleteUser(context.Context, string) error { return nil }

func (*fakeIdentity) GetMyBilling(context.Context) (domain.BillingAddress, error) {
	return domain.BillingAddress{}, ycerrors.NotFound("Billing", userID)
}

func (*fakeIdentity) PutMyBilling(_ context.Context, in domain.BillingAddress) (domain.BillingAddress, error) {
	return in, nil
}

func (*fakeIdentity) DeleteMyBilling(context.Context) error { return nil }

func (*fakeIdentity) UserBilling(context.Context, string) (domain.BillingAddress, error) {
	return domain.BillingAddress{}, ycerrors.NotFound("Billing", userID)
}

func (*fakeIdentity) ListMyAddresses(context.Context, bool) ([]domain.DeliveryAddress, error) {
	return nil, nil
}

func (*fakeIdentity) GetMyAddress(context.Context, string) (domain.DeliveryAddress, error) {
	return domain.DeliveryAddress{}, ycerrors.NotFound("Address", userID)
}

func (*fakeIdentity) CreateMyAddress(_ context.Context, in domain.DeliveryAddressInput) (domain.DeliveryAddress, error) {
	return domain.DeliveryAddress{ID: userID, Address: in.Address, Label: in.Label, IsDefault: true}, nil
}

func (*fakeIdentity) PutMyAddress(_ context.Context, id string, in domain.DeliveryAddressInput) (domain.DeliveryAddress, error) {
	return domain.DeliveryAddress{ID: id, Address: in.Address, Label: in.Label}, nil
}

func (*fakeIdentity) DeleteMyAddress(context.Context, string) error { return nil }

func (*fakeIdentity) ListUserAddresses(context.Context, string, bool) ([]domain.DeliveryAddress, error) {
	return nil, nil
}

func (*fakeIdentity) GetUserAddress(context.Context, string, string) (domain.DeliveryAddress, error) {
	return domain.DeliveryAddress{}, ycerrors.NotFound("Address", userID)
}

func (*fakeIdentity) UserAddress(context.Context, string, string) (domain.DeliveryAddress, error) {
	return domain.DeliveryAddress{}, ycerrors.NotFound("Address", userID)
}

func (*fakeIdentity) CreateUserAddress(_ context.Context, _ string, in domain.DeliveryAddressInput) (domain.DeliveryAddress, error) {
	return domain.DeliveryAddress{ID: userID, Address: in.Address, Label: in.Label, IsDefault: true}, nil
}

func (*fakeIdentity) CreateInvitation(_ context.Context, in domain.InviteInput) (domain.Invitation, error) {
	return domain.Invitation{ID: userID, TenantID: in.TenantID, Email: in.Email, Role: in.Role, Status: domain.InvitationPending}, nil
}

func (*fakeIdentity) ListInvitations(context.Context, repository.InvitationFilter, *repository.InvitationCursor, int) ([]domain.Invitation, *repository.InvitationCursor, error) {
	return nil, nil, nil
}

func (*fakeIdentity) ResendInvitation(_ context.Context, id string) (domain.Invitation, error) {
	return domain.Invitation{ID: id, Status: domain.InvitationPending}, nil
}

func (*fakeIdentity) RevokeInvitation(context.Context, string) error { return nil }

func (*fakeIdentity) AcceptInvitation(context.Context, domain.AcceptInviteInput) (domain.Session, error) {
	return domain.Session{Token: "admin", UserID: userID, TenantID: "_platform", Role: domain.RoleSystemAdmin}, nil
}

func serve(t *testing.T, f *fakeIdentity, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	transport.NewRouter(f, nil, httpkit.BuildInfo{Service: "yc-identity"}, nil).ServeHTTP(rec, req)
	return rec
}

func as(req *http.Request, tenant, actorType string) *http.Request {
	req.Header.Set(authctx.HeaderTenantID, tenant)
	if actorType != "" {
		req.Header.Set(authctx.HeaderActorID, userID)
		req.Header.Set(authctx.HeaderActorType, actorType)
	}
	return req
}

func TestRoutes(t *testing.T) {
	telemetry.InitForTest(t, "yc-identity")

	t.Run("visitor uses the gateway's tenant header", func(t *testing.T) {
		f := &fakeIdentity{}
		rec := serve(t, f, as(httptest.NewRequest(http.MethodPost, "/visitor", nil), "shop-a", ""))
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		assert.Equal(t, "shop-a", f.tenant)
		var body api.Session
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, "tok", body.AccessToken)
		assert.Equal(t, api.SessionTokenType("Bearer"), body.TokenType)
		assert.Equal(t, api.Role("visitor"), body.Role)
	})

	t.Run("visitor without tenant is a validation error", func(t *testing.T) {
		rec := serve(t, &fakeIdentity{}, httptest.NewRequest(http.MethodPost, "/visitor", nil))
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, ycerrors.ContentType, rec.Header().Get("Content-Type"))
	})

	t.Run("register normalises the email", func(t *testing.T) {
		f := &fakeIdentity{}
		req := as(httptest.NewRequest(http.MethodPost, "/register",
			strings.NewReader(`{"email":" A@B.co ","password":"long-enough-pw","display_name":"Ann"}`)), "shop-a", "visitor")
		rec := serve(t, f, req)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		assert.Equal(t, domain.Registration{Email: "a@b.co", Password: "long-enough-pw", DisplayName: "Ann"}, f.reg)
	})

	t.Run("register rejects a short password", func(t *testing.T) {
		req := as(httptest.NewRequest(http.MethodPost, "/register",
			strings.NewReader(`{"email":"a@b.co","password":"short"}`)), "shop-a", "visitor")
		rec := serve(t, &fakeIdentity{}, req)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "password")
	})

	t.Run("register rejects malformed JSON", func(t *testing.T) {
		req := as(httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(`{`)), "shop-a", "visitor")
		assert.Equal(t, http.StatusBadRequest, serve(t, &fakeIdentity{}, req).Code)
	})

	t.Run("login", func(t *testing.T) {
		ok := serve(t, &fakeIdentity{}, as(httptest.NewRequest(http.MethodPost, "/login",
			strings.NewReader(`{"email":"a@b.co","password":"secret-password"}`)), "shop-a", ""))
		require.Equal(t, http.StatusOK, ok.Code, ok.Body.String())
		bad := serve(t, &fakeIdentity{}, as(httptest.NewRequest(http.MethodPost, "/login",
			strings.NewReader(`{"email":"a@b.co","password":"nope"}`)), "shop-a", ""))
		assert.Equal(t, http.StatusUnauthorized, bad.Code)
	})

	t.Run("validate token", func(t *testing.T) {
		rec := serve(t, &fakeIdentity{}, internal(httptest.NewRequest(http.MethodPost, "/internal/validate-token",
			strings.NewReader(`{"token":"good"}`))))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, "shop-a", body["tenant_id"])
		assert.Equal(t, userID, body["actor_id"])
		assert.Equal(t, "visitor", body["actor_type"])
		assert.Equal(t, jti, body["jti"])
	})

	t.Run("validate service token", func(t *testing.T) {
		rec := serve(t, &fakeIdentity{}, internal(httptest.NewRequest(http.MethodPost, "/internal/validate-token",
			strings.NewReader(`{"token":"svc"}`))))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, "yc-cart", body["actor_id"])
		assert.Equal(t, "service", body["actor_type"])
	})

	t.Run("internal routes need a caller service", func(t *testing.T) {
		rec := serve(t, &fakeIdentity{}, httptest.NewRequest(http.MethodPost, "/internal/validate-token",
			strings.NewReader(`{"token":"good"}`)))
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("invalid token is 401", func(t *testing.T) {
		rec := serve(t, &fakeIdentity{}, internal(httptest.NewRequest(http.MethodPost, "/internal/validate-token",
			strings.NewReader(`{"token":"bad"}`))))
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("missing token is 400", func(t *testing.T) {
		rec := serve(t, &fakeIdentity{}, internal(httptest.NewRequest(http.MethodPost, "/internal/validate-token",
			strings.NewReader(`{}`))))
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("service token", func(t *testing.T) {
		rec := serve(t, &fakeIdentity{}, internal(httptest.NewRequest(http.MethodPost, "/internal/service-token",
			strings.NewReader(`{"client_id":"yc-cart","client_secret":"s"}`))))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body api.ServiceToken
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, "svc", body.AccessToken)
		bad := serve(t, &fakeIdentity{}, internal(httptest.NewRequest(http.MethodPost, "/internal/service-token",
			strings.NewReader(`{"client_id":"yc-cart","client_secret":"x"}`))))
		assert.Equal(t, http.StatusUnauthorized, bad.Code)
	})

	t.Run("refresh returns the rotated token", func(t *testing.T) {
		rec := serve(t, &fakeIdentity{}, as(httptest.NewRequest(http.MethodPost, "/refresh",
			strings.NewReader(`{"refresh_token":"r1"}`)), "shop-a", ""))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body api.Session
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.NotNil(t, body.RefreshToken)
		assert.Equal(t, "r2", *body.RefreshToken)
		assert.NotNil(t, body.RefreshExpiresAt)

		empty := serve(t, &fakeIdentity{}, as(httptest.NewRequest(http.MethodPost, "/refresh",
			strings.NewReader(`{"refresh_token":""}`)), "shop-a", ""))
		assert.Equal(t, http.StatusBadRequest, empty.Code)
	})

	t.Run("logout with and without a body", func(t *testing.T) {
		f := &fakeIdentity{}
		rec := serve(t, f, as(httptest.NewRequest(http.MethodPost, "/logout",
			strings.NewReader(`{"refresh_token":"r1"}`)), "shop-a", "customer"))
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		assert.Equal(t, "r1", f.refresh)
		req := as(httptest.NewRequest(http.MethodPost, "/logout", http.NoBody), "shop-a", "customer")
		assert.Equal(t, http.StatusNoContent, serve(t, &fakeIdentity{}, req).Code)
	})

	t.Run("password reset", func(t *testing.T) {
		f := &fakeIdentity{}
		rec := serve(t, f, as(httptest.NewRequest(http.MethodPost, "/password/reset-request",
			strings.NewReader(`{"email":"a@b.co"}`)), "shop-a", ""))
		assert.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
		assert.Equal(t, "a@b.co", f.email)
		rec = serve(t, f, as(httptest.NewRequest(http.MethodPost, "/password/reset-confirm",
			strings.NewReader(`{"token":"t","password":"a-new-password"}`)), "shop-a", ""))
		assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		assert.Equal(t, "t/a-new-password", f.reset)
	})

	t.Run("oauth start uses the forwarded host", func(t *testing.T) {
		f := &fakeIdentity{}
		req := as(httptest.NewRequest(http.MethodPost, "/oauth/google/start",
			strings.NewReader(`{"login_hint":"a@b.co"}`)), "shop-a", "visitor")
		req.Header.Set("X-Forwarded-Host", "shop-a.test")
		rec := serve(t, f, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, "google shop-a.test a@b.co", f.oauth)
		assert.Contains(t, rec.Body.String(), "https://provider.test/auth")
	})

	t.Run("oauth callback", func(t *testing.T) {
		f := &fakeIdentity{}
		rec := serve(t, f, as(httptest.NewRequest(http.MethodPost, "/oauth/facebook/callback",
			strings.NewReader(`{"code":"c","state":"s"}`)), "shop-a", "visitor"))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, "facebook c s", f.oauth)
	})

	t.Run("me requires an actor", func(t *testing.T) {
		rec := serve(t, &fakeIdentity{}, httptest.NewRequest(http.MethodGet, "/me", nil))
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("me returns the caller", func(t *testing.T) {
		rec := serve(t, &fakeIdentity{}, as(httptest.NewRequest(http.MethodGet, "/me", nil), "shop-a", "visitor"))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body api.User
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, userID, body.UserID.String())
		assert.Equal(t, api.Role("visitor"), body.Role)
	})

	t.Run("admin routes need an admin", func(t *testing.T) {
		rec := serve(t, &fakeIdentity{}, as(httptest.NewRequest(http.MethodGet, "/admin/users", nil), "shop-a", "customer"))
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("list users pages with a cursor", func(t *testing.T) {
		f := &fakeIdentity{}
		cursor := httpkit.EncodeCursor(repository.Cursor{ID: "x"})
		rec := serve(t, f, as(httptest.NewRequest(http.MethodGet,
			"/admin/users?page_size=5&role=customer&email=A@B.co&cursor="+cursor, nil), "_platform", "system_admin"))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, 5, f.limit)
		assert.Equal(t, repository.Filter{Email: "a@b.co", Role: domain.RoleCustomer}, f.filter)
		require.NotNil(t, f.after)
		assert.Equal(t, "x", f.after.ID)
		var body api.UserList
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.Len(t, body.Items, 1)
		assert.NotEmpty(t, body.Page.NextCursor)
	})

	t.Run("list users rejects a bad page size", func(t *testing.T) {
		rec := serve(t, &fakeIdentity{}, as(httptest.NewRequest(http.MethodGet, "/admin/users?page_size=500", nil), "shop-a", "shop_admin"))
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("get user rejects a non-UUID id", func(t *testing.T) {
		rec := serve(t, &fakeIdentity{}, as(httptest.NewRequest(http.MethodGet, "/admin/users/nope", nil), "shop-a", "shop_admin"))
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, ycerrors.ContentType, rec.Header().Get("Content-Type"))
	})

	t.Run("promote", func(t *testing.T) {
		f := &fakeIdentity{}
		rec := serve(t, f, as(httptest.NewRequest(http.MethodPost, "/admin/users/"+userID+"/promote",
			strings.NewReader(`{"role":"shop_admin"}`)), "_platform", "system_admin"))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, domain.RoleShopAdmin, f.promoted)
	})
}
