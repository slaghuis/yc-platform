package httpkit_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
)

func TestInternalClient(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	issued, rejectNext := 0, false
	var seen http.Header
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/internal/service-token":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["client_id"] != "yc-cart" || body["client_secret"] != "s3cret" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			issued++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "svc-" + string(rune('0'+issued)),
				"expires_at":   time.Now().Add(15 * time.Minute),
			})
		case "/internal/evaluate":
			if rejectNext {
				rejectNext = false
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			seen = r.Header.Clone()
			_ = json.NewEncoder(w).Encode(map[string]string{"ok": "yes"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer gw.Close()

	c := httpkit.NewInternalClient("yc-cart", httpkit.InternalConfig{GatewayURL: gw.URL, ClientSecret: "s3cret"})
	ctx := authctx.WithAuth(context.Background(), authctx.AuthContext{
		TenantID: "shop-a", ActorID: "u1", ActorType: authctx.ActorCustomer,
		CorrelationID: "corr-1", Token: "user-token",
	})

	var out map[string]string
	require.NoError(t, c.Do(ctx, http.MethodPost, "/internal/evaluate", map[string]int{"n": 1}, &out))
	assert.Equal(t, "yes", out["ok"])
	assert.Equal(t, "Bearer svc-1", seen.Get("Authorization"), "service token, never the user's")
	assert.Equal(t, "shop-a", seen.Get(authctx.HeaderTenantID))
	assert.Equal(t, "u1", seen.Get(authctx.HeaderActorID))
	assert.Equal(t, "customer", seen.Get(authctx.HeaderActorType))
	assert.Equal(t, "corr-1", seen.Get(authctx.HeaderCorrelationID))

	require.NoError(t, c.Do(ctx, http.MethodPost, "/internal/evaluate", nil, nil))
	mu.Lock()
	assert.Equal(t, 1, issued, "token is cached")
	rejectNext = true
	mu.Unlock()

	require.NoError(t, c.Do(ctx, http.MethodPost, "/internal/evaluate", nil, nil))
	mu.Lock()
	assert.Equal(t, 2, issued, "a 401 fetches a new token once")
	assert.Equal(t, "Bearer svc-2", seen.Get("Authorization"))
	mu.Unlock()

	bad := httpkit.NewInternalClient("yc-cart", httpkit.InternalConfig{GatewayURL: gw.URL, ClientSecret: "wrong"})
	assert.Equal(t, http.StatusUnauthorized, httpkit.UpstreamStatus(bad.Do(ctx, http.MethodPost, "/internal/evaluate", nil, nil)))

	assert.Error(t, c.Do(context.Background(), http.MethodPost, "/internal/evaluate", nil, nil), "tenant required")
}
