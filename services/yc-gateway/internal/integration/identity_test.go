package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	"github.com/slaghuis/yc-platform/services/yc-gateway/internal/integration"
)

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func fakeIdentity(t *testing.T, calls *atomic.Int32, tenantHeader *atomic.Value) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/internal/validate-token":
			var req struct{ Token string }
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			if req.Token != "good" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tenant_id": "shop-a", "actor_id": "u1", "actor_type": "customer", "jti": "j1",
				"expires_at": time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
			})
		case "/visitor":
			tenantHeader.Store(r.Header.Get(authctx.HeaderTenantID))
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "vis", "token_type": "Bearer", "user_id": "v1", "tenant_id": "shop-a",
				"expires_at": time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
			})
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestValidateCaches(t *testing.T) {
	telemetry.InitForTest(t, "yc-gateway")
	var calls atomic.Int32
	clk := &clock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	c := integration.NewIdentity(fakeIdentity(t, &calls, &atomic.Value{}), time.Second, time.Minute, clk.Now)
	ctx := context.Background()

	id, err := c.Validate(ctx, "good")
	require.NoError(t, err)
	assert.Equal(t, "shop-a", id.TenantID)
	assert.Equal(t, authctx.ActorCustomer, id.ActorType)
	assert.Equal(t, "j1", id.TokenID)
	assert.Equal(t, "good", id.Token)

	_, err = c.Validate(ctx, "good")
	require.NoError(t, err)
	assert.Equal(t, int32(1), calls.Load(), "second call served from cache")

	clk.now = clk.now.Add(time.Minute)
	_, err = c.Validate(ctx, "good")
	require.NoError(t, err)
	assert.Equal(t, int32(2), calls.Load(), "expired entry revalidated")
}

func TestValidateInvalid(t *testing.T) {
	telemetry.InitForTest(t, "yc-gateway")
	var calls atomic.Int32
	c := integration.NewIdentity(fakeIdentity(t, &calls, &atomic.Value{}), time.Second, time.Minute, time.Now)

	_, err := c.Validate(context.Background(), "bad")
	require.ErrorIs(t, err, integration.ErrInvalidToken)
	_, err = c.Validate(context.Background(), "bad")
	require.ErrorIs(t, err, integration.ErrInvalidToken)
	assert.Equal(t, int32(2), calls.Load(), "rejections are not cached")
}

func TestValidateUpstreamDown(t *testing.T) {
	telemetry.InitForTest(t, "yc-gateway")
	c := integration.NewIdentity("http://127.0.0.1:1", time.Second, time.Minute, time.Now)

	_, err := c.Validate(context.Background(), "good")
	require.Error(t, err)
	assert.NotErrorIs(t, err, integration.ErrInvalidToken)
	assert.Zero(t, httpkit.UpstreamStatus(err))
}

func TestCreateVisitor(t *testing.T) {
	telemetry.InitForTest(t, "yc-gateway")
	var calls atomic.Int32
	var tenant atomic.Value
	c := integration.NewIdentity(fakeIdentity(t, &calls, &tenant), time.Second, time.Minute, time.Now)
	ctx := authctx.WithCorrelationID(context.Background(), "corr-1")

	id, err := c.CreateVisitor(ctx, "shop-a")
	require.NoError(t, err)
	assert.Equal(t, "shop-a", tenant.Load())
	assert.Equal(t, "vis", id.Token)
	assert.Equal(t, authctx.ActorVisitor, id.ActorType)

	got, err := c.Validate(ctx, "vis")
	require.NoError(t, err)
	assert.Equal(t, "v1", got.ActorID)
	assert.Equal(t, int32(1), calls.Load(), "issued token is pre-cached")
}
