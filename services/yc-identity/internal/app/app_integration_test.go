//go:build integration

package app_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
	testkit "github.com/slaghuis/yc-platform/libs/yc-testkit"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/app"
	transport "github.com/slaghuis/yc-platform/services/yc-identity/internal/transport/http"
)

func writeKey(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "jwt-private.pem")
	b := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	require.NoError(t, os.WriteFile(path, b, 0o600))
	return path
}

func call(t *testing.T, srv *httptest.Server, method, path string, body any, hdr map[string]string, out any) int {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req, err := http.NewRequestWithContext(context.Background(), method, srv.URL+path, &buf)
	require.NoError(t, err)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	if out != nil && resp.StatusCode < 300 {
		require.NoError(t, json.NewDecoder(resp.Body).Decode(out))
	}
	return resp.StatusCode
}

func TestIdentity(t *testing.T) {
	ctx := context.Background()
	telemetry.InitForTest(t, "yc-identity")

	dbURL := testkit.Postgres(t)
	natsURL := testkit.NATS(t)

	t.Setenv("YC_IDENTITY_DB_URL", dbURL)
	t.Setenv("NATS_URL", natsURL)
	t.Setenv("YC_IDENTITY_JWT_PRIVATE_KEY_FILE", writeKey(t))
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	cfg, err := app.LoadConfig()
	require.NoError(t, err)

	a, err := app.New(ctx, cfg, httpkit.BuildInfo{Service: "yc-identity", Version: "test"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	srv := httptest.NewServer(a.Handler)
	t.Cleanup(srv.Close)

	db, err := pgxpool.New(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(db.Close)

	var visitor transport.SessionResponse
	corr := "0192f3a4-5b6c-7d8e-9f01-23456789abcd"
	t.Run("visitor is created, published and valid", func(t *testing.T) {
		code := call(t, srv, http.MethodPost, "/visitor", nil, map[string]string{
			authctx.HeaderTenantID:      "shop-a",
			authctx.HeaderCorrelationID: corr,
		}, &visitor)
		require.Equal(t, http.StatusCreated, code)
		assert.Equal(t, "shop-a", visitor.TenantID)
		assert.WithinDuration(t, time.Now().Add(cfg.JWT.VisitorTokenTTL), visitor.ExpiresAt, time.Minute)

		nc, err := nats.Connect(natsURL)
		require.NoError(t, err)
		defer nc.Close()
		js, err := jetstream.New(nc)
		require.NoError(t, err)
		stream, err := js.Stream(ctx, "YC_IDENTITY")
		require.NoError(t, err)
		var msg *jetstream.RawStreamMsg
		require.Eventually(t, func() bool {
			msg, err = stream.GetLastMsgForSubject(ctx, "yc.shop-a.identity.visitor.created.v1")
			return err == nil
		}, 10*time.Second, 50*time.Millisecond)

		var env eventbus.Envelope
		require.NoError(t, json.Unmarshal(msg.Data, &env))
		assert.Equal(t, "shop-a", env.TenantID)
		assert.Equal(t, visitor.UserID, env.Actor.ID)
		assert.Equal(t, "visitor", env.Actor.Type)
		assert.NotEmpty(t, env.Actor.AccessTokenJTI)
		assert.Equal(t, corr, env.CorrelationID, "event carries the request's correlation ID")
		assert.Equal(t, "yc-identity", env.SourceService)
		var payload events.IdentityVisitorCreatedV1
		require.NoError(t, json.Unmarshal(env.Payload, &payload))
		assert.Equal(t, visitor.UserID, payload.UserID)
	})

	t.Run("validate-token returns the identity", func(t *testing.T) {
		var got transport.ValidateResponse
		code := call(t, srv, http.MethodPost, "/internal/validate-token",
			transport.ValidateRequest{Token: visitor.AccessToken}, nil, &got)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, "shop-a", got.TenantID)
		assert.Equal(t, visitor.UserID, got.ActorID)
		assert.Equal(t, "visitor", got.ActorType)
	})

	headers := func(tenant string) map[string]string {
		return map[string]string{
			authctx.HeaderTenantID:  tenant,
			authctx.HeaderActorID:   visitor.UserID,
			authctx.HeaderActorType: "visitor",
		}
	}

	t.Run("me returns the visitor", func(t *testing.T) {
		var me transport.MeResponse
		require.Equal(t, http.StatusOK, call(t, srv, http.MethodGet, "/me", nil, headers("shop-a"), &me))
		assert.Equal(t, visitor.UserID, me.UserID)
		assert.Equal(t, "active", me.Status)
	})

	t.Run("another tenant cannot see the visitor", func(t *testing.T) {
		assert.Equal(t, http.StatusNotFound, call(t, srv, http.MethodGet, "/me", nil, headers("shop-b"), nil))
	})

	t.Run("platform tenant cannot create visitors", func(t *testing.T) {
		code := call(t, srv, http.MethodPost, "/visitor", nil, map[string]string{authctx.HeaderTenantID: "_platform"}, nil)
		assert.Equal(t, http.StatusBadRequest, code)
	})

	t.Run("suspended user's token is rejected", func(t *testing.T) {
		require.NoError(t, dbkit.WithTenantTx(ctx, db, "shop-a", func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE users SET status = 'suspended' WHERE id = $1`, visitor.UserID)
			return err
		}))
		code := call(t, srv, http.MethodPost, "/internal/validate-token",
			transport.ValidateRequest{Token: visitor.AccessToken}, nil, nil)
		assert.Equal(t, http.StatusUnauthorized, code)
	})

	t.Run("readyz reports dependencies", func(t *testing.T) {
		var r httpkit.Readiness
		require.Equal(t, http.StatusOK, call(t, srv, http.MethodGet, "/readyz", nil, nil, &r))
		assert.Equal(t, map[string]string{"postgres": "ok", "nats": "ok"}, r.Checks)
	})
}
