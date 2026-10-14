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
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/domain"
	transport "github.com/slaghuis/yc-platform/services/yc-identity/internal/transport/http"
)

type fakeIdentity struct {
	tenant string
}

func (f *fakeIdentity) CreateVisitor(_ context.Context, tenantID string) (domain.Session, error) {
	f.tenant = tenantID
	if tenantID == "" {
		return domain.Session{}, ycerrors.Validation().Field("tenant_id", "required", "Tenant is required").Build()
	}
	return domain.Session{Token: "tok", UserID: "u-1", TenantID: tenantID, ExpiresAt: time.Unix(1700000000, 0)}, nil
}

func (f *fakeIdentity) ValidateToken(_ context.Context, raw string) (domain.Session, error) {
	if raw != "good" {
		return domain.Session{}, ycerrors.Unauthorized("Invalid or expired token")
	}
	return domain.Session{TokenID: "j-1", UserID: "u-1", TenantID: "shop-a", Role: domain.RoleVisitor}, nil
}

func (f *fakeIdentity) Me(ctx context.Context) (domain.User, error) {
	a, _ := authctx.FromContext(ctx)
	return domain.User{ID: a.ActorID, TenantID: a.TenantID, Role: domain.Role(a.ActorType), Status: domain.StatusActive}, nil
}

func serve(t *testing.T, f *fakeIdentity, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	transport.NewRouter(f, httpkit.BuildInfo{Service: "yc-identity"}, nil).ServeHTTP(rec, req)
	return rec
}

func TestRoutes(t *testing.T) {
	telemetry.InitForTest(t, "yc-identity")

	t.Run("visitor uses the gateway's tenant header", func(t *testing.T) {
		f := &fakeIdentity{}
		req := httptest.NewRequest(http.MethodPost, "/visitor", nil)
		req.Header.Set(authctx.HeaderTenantID, "shop-a")
		rec := serve(t, f, req)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		assert.Equal(t, "shop-a", f.tenant)
		var body transport.SessionResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, "tok", body.AccessToken)
		assert.Equal(t, "Bearer", body.TokenType)
	})

	t.Run("visitor without tenant is a validation error", func(t *testing.T) {
		rec := serve(t, &fakeIdentity{}, httptest.NewRequest(http.MethodPost, "/visitor", nil))
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, ycerrors.ContentType, rec.Header().Get("Content-Type"))
	})

	t.Run("validate token", func(t *testing.T) {
		rec := serve(t, &fakeIdentity{}, httptest.NewRequest(http.MethodPost, "/internal/validate-token",
			strings.NewReader(`{"token":"good"}`)))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body transport.ValidateResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, transport.ValidateResponse{TenantID: "shop-a", ActorID: "u-1", ActorType: "visitor", TokenID: "j-1", ExpiresAt: body.ExpiresAt}, body)
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
		req := httptest.NewRequest(http.MethodGet, "/me", nil)
		req.Header.Set(authctx.HeaderTenantID, "shop-a")
		req.Header.Set(authctx.HeaderActorID, "u-1")
		req.Header.Set(authctx.HeaderActorType, "visitor")
		rec := serve(t, &fakeIdentity{}, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body transport.MeResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, "u-1", body.UserID)
		assert.Equal(t, "visitor", body.Role)
	})
}
