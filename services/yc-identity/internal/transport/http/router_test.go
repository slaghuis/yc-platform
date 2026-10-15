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

func (f *fakeIdentity) ValidateToken(_ context.Context, raw string) (domain.Session, error) {
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

func serve(t *testing.T, f *fakeIdentity, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	transport.NewRouter(f, httpkit.BuildInfo{Service: "yc-identity"}, nil).ServeHTTP(rec, req)
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
		rec := serve(t, &fakeIdentity{}, httptest.NewRequest(http.MethodPost, "/internal/validate-token",
			strings.NewReader(`{"token":"good"}`)))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, "shop-a", body["tenant_id"])
		assert.Equal(t, userID, body["actor_id"])
		assert.Equal(t, "visitor", body["actor_type"])
		assert.Equal(t, jti, body["jti"])
	})

	t.Run("invalid token is 401", func(t *testing.T) {
		rec := serve(t, &fakeIdentity{}, httptest.NewRequest(http.MethodPost, "/internal/validate-token",
			strings.NewReader(`{"token":"bad"}`)))
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("missing token is 400", func(t *testing.T) {
		rec := serve(t, &fakeIdentity{}, httptest.NewRequest(http.MethodPost, "/internal/validate-token",
			strings.NewReader(`{}`)))
		assert.Equal(t, http.StatusBadRequest, rec.Code)
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
