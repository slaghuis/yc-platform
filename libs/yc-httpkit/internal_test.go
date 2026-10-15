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
	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
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

func TestInternalClientCall(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var keys []string
	statuses := map[string][]int{}
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/service-token" {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "t", "expires_at": time.Now().Add(time.Hour)})
			return
		}
		mu.Lock()
		keys = append(keys, r.Header.Get(httpkit.HeaderIdempotencyKey))
		queue := statuses[r.URL.Path]
		status := http.StatusOK
		if len(queue) > 0 {
			status, statuses[r.URL.Path] = queue[0], queue[1:]
		}
		mu.Unlock()
		if status == 0 {
			time.Sleep(300 * time.Millisecond)
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer gw.Close()
	c := httpkit.NewInternalClient("yc-checkout", httpkit.InternalConfig{GatewayURL: gw.URL, ClientSecret: "s"})
	ctx := authctx.WithAuth(context.Background(), authctx.AuthContext{TenantID: "shop-a"})
	opts := httpkit.CallOptions{IdempotencyKey: "sess:reserve", Attempts: 3, Backoff: time.Millisecond, Timeout: 100 * time.Millisecond}

	mu.Lock()
	statuses["/a"] = []int{http.StatusBadGateway, 0, http.StatusOK}
	mu.Unlock()
	require.NoError(t, c.Call(ctx, http.MethodPost, "/a", opts, nil, nil), "a 5xx and a timeout are retried")
	mu.Lock()
	assert.Equal(t, []string{"sess:reserve", "sess:reserve", "sess:reserve"}, keys, "the same key on every attempt")
	keys = nil
	statuses["/b"] = []int{http.StatusConflict}
	mu.Unlock()

	err := c.Call(ctx, http.MethodPost, "/b", opts, nil, nil)
	assert.Equal(t, http.StatusConflict, httpkit.UpstreamStatus(err))
	mu.Lock()
	assert.Len(t, keys, 1, "a 4xx is not retried")
	keys = nil
	statuses["/c"] = []int{500, 500, 500, 200}
	mu.Unlock()

	err = c.Call(ctx, http.MethodPost, "/c", opts, nil, nil)
	assert.Equal(t, http.StatusInternalServerError, httpkit.UpstreamStatus(err), "attempts run out")
	mu.Lock()
	assert.Len(t, keys, 3)
	mu.Unlock()
	_, ok := httpkit.UpstreamProblem(err)
	assert.False(t, ok, "not a problem body")
}

func TestUpstreamProblem(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ycerrors.WriteError(w, r, ycerrors.New(ycerrors.CodeInsufficientStock, "Short").
			With("shortages", []map[string]any{{"sku_id": "a", "available": 1}}))
	}))
	defer srv.Close()
	err := httpkit.DoJSON(context.Background(), srv.Client(), http.MethodPost, srv.URL, nil, nil)
	p, ok := httpkit.UpstreamProblem(err)
	require.True(t, ok)
	assert.Equal(t, ycerrors.CodeInsufficientStock, p.Code)
	assert.Equal(t, []any{map[string]any{"sku_id": "a", "available": float64(1)}}, p.Extensions["shortages"])
	_, ok = httpkit.UpstreamProblem(context.Canceled)
	assert.False(t, ok)
}
