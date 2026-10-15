//go:build integration

package httpkit_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	testkit "github.com/slaghuis/yc-platform/libs/yc-testkit"
)

func TestIdempotency(t *testing.T) {
	ctx := context.Background()
	db, err := pgxpool.New(ctx, testkit.Postgres(t))
	require.NoError(t, err)
	t.Cleanup(db.Close)
	_, err = db.Exec(ctx, httpkit.IdempotencySchema)
	require.NoError(t, err)
	store := httpkit.PGIdempotencyStore{DB: db}

	var calls atomic.Int32
	release := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		switch r.URL.Path {
		case "/slow":
			<-release
		case "/fail":
			if n == 1 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
		case "/reject":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"n":` + fmt.Sprint(n) + `}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"n":` + fmt.Sprint(n) + `}`))
	})
	auth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(authctx.WithAuth(r.Context(), authctx.AuthContext{
				TenantID: r.Header.Get(authctx.HeaderTenantID), ActorID: "svc", ActorType: authctx.ActorService,
			})))
		})
	}
	srv := httptest.NewServer(auth(httpkit.Idempotency(httpkit.IdempotencyOptions{Store: store, Required: true})(h)))
	t.Cleanup(srv.Close)

	post := func(path, tenant, key, body string) (int, string, http.Header) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+path, strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set(authctx.HeaderTenantID, tenant)
		req.Header.Set(authctx.HeaderCallerService, "yc-checkout")
		if key != "" {
			req.Header.Set(httpkit.HeaderIdempotencyKey, key)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, string(b), resp.Header
	}

	t.Run("replays the stored response", func(t *testing.T) {
		calls.Store(0)
		status, body, _ := post("/ok", "shop-a", "k1", `{"a":1}`)
		assert.Equal(t, http.StatusCreated, status)
		assert.JSONEq(t, `{"n":1}`, body)
		status, body, hdr := post("/ok", "shop-a", "k1", `{"a":1}`)
		assert.Equal(t, http.StatusCreated, status)
		assert.JSONEq(t, `{"n":1}`, body)
		assert.Equal(t, "true", hdr.Get(httpkit.HeaderIdempotentReplayed))
		assert.Equal(t, "application/json", hdr.Get("Content-Type"))
		assert.EqualValues(t, 1, calls.Load())

		status, _, _ = post("/ok", "shop-a", "k1", `{"a":2}`)
		assert.Equal(t, http.StatusUnprocessableEntity, status, "same key, different body")
		status, _, _ = post("/ok", "shop-b", "k1", `{"a":1}`)
		assert.Equal(t, http.StatusCreated, status, "keys are per tenant")
		status, _, _ = post("/other", "shop-a", "k1", `{"a":1}`)
		assert.Equal(t, http.StatusCreated, status, "keys are per path")
		status, _, _ = post("/ok", "shop-a", "", `{}`)
		assert.Equal(t, http.StatusBadRequest, status, "key required")
	})

	t.Run("4xx is stored, 5xx is not", func(t *testing.T) {
		calls.Store(0)
		status, body, _ := post("/reject", "shop-a", "k2", `{}`)
		assert.Equal(t, http.StatusConflict, status)
		_, body2, _ := post("/reject", "shop-a", "k2", `{}`)
		assert.Equal(t, body, body2)
		assert.EqualValues(t, 1, calls.Load())

		calls.Store(0)
		status, _, _ = post("/fail", "shop-a", "k3", `{}`)
		assert.Equal(t, http.StatusBadGateway, status)
		status, body, _ = post("/fail", "shop-a", "k3", `{}`)
		assert.Equal(t, http.StatusCreated, status, "a 5xx is retried")
		assert.JSONEq(t, `{"n":2}`, body)
	})

	t.Run("a running request holds its key", func(t *testing.T) {
		done := make(chan int)
		go func() {
			status, _, _ := post("/slow", "shop-a", "k4", `{}`)
			done <- status
		}()
		require.Eventually(t, func() bool {
			var n int
			_ = db.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE key = 'k4'`).Scan(&n)
			return n == 1
		}, 5*time.Second, 20*time.Millisecond)
		status, _, _ := post("/slow", "shop-a", "k4", `{}`)
		assert.Equal(t, http.StatusConflict, status)
		close(release)
		assert.Equal(t, http.StatusCreated, <-done)
	})

	t.Run("an expired claim can be taken again", func(t *testing.T) {
		_, err := db.Exec(ctx, `INSERT INTO idempotency_keys (scope, key, request_hash, locked_until)
			VALUES ('s', 'k5', '\x01', NOW() - INTERVAL '1 second')`)
		require.NoError(t, err)
		c, err := store.Claim(ctx, "s", "k5", []byte{1}, time.Minute)
		require.NoError(t, err)
		assert.True(t, c.Claimed)
		c, err = store.Claim(ctx, "s", "k5", []byte{1}, time.Minute)
		require.NoError(t, err)
		assert.False(t, c.Claimed)
		assert.Nil(t, c.Response)
	})

	t.Run("purge", func(t *testing.T) {
		_, err := db.Exec(ctx, `UPDATE idempotency_keys SET created_at = NOW() - INTERVAL '2 days' WHERE key = 'k1'`)
		require.NoError(t, err)
		n, err := store.Purge(ctx, 24*time.Hour)
		require.NoError(t, err)
		assert.EqualValues(t, 3, n)
	})
}
