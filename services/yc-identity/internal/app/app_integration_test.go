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
	api "github.com/slaghuis/yc-platform/services/yc-identity/api/generated"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/app"
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

// as returns the headers yc-gateway would set for an actor.
func as(tenant, actorID, actorType string) map[string]string {
	h := map[string]string{authctx.HeaderTenantID: tenant}
	if actorID != "" {
		h[authctx.HeaderActorID] = actorID
		h[authctx.HeaderActorType] = actorType
	}
	return h
}

type validated struct {
	TenantID  string `json:"tenant_id"`
	ActorID   string `json:"actor_id"`
	ActorType string `json:"actor_type"`
}

func TestIdentity(t *testing.T) {
	ctx := context.Background()
	logs := telemetry.InitForTest(t, "yc-identity")
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	})

	dbURL := testkit.Postgres(t)
	natsURL := testkit.NATS(t)

	t.Setenv("YC_IDENTITY_DB_URL", dbURL)
	t.Setenv("NATS_URL", natsURL)
	t.Setenv("YC_IDENTITY_JWT_PRIVATE_KEY_FILE", writeKey(t))
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("YC_IDENTITY__PASSWORD__ARGON2__TIME", "1")
	t.Setenv("YC_IDENTITY__PASSWORD__ARGON2__MEMORY_KB", "8192")
	cfg, err := app.LoadConfig()
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())

	a, err := app.New(ctx, cfg, httpkit.BuildInfo{Service: "yc-identity", Version: "test"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	srv := httptest.NewServer(a.Handler)
	t.Cleanup(srv.Close)

	db, err := pgxpool.New(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(db.Close)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	lastEvent := func(t *testing.T, subject string) eventbus.Envelope {
		t.Helper()
		var msg *jetstream.RawStreamMsg
		require.Eventually(t, func() bool {
			stream, err := js.Stream(ctx, "YC_IDENTITY")
			if err != nil {
				return false
			}
			msg, err = stream.GetLastMsgForSubject(ctx, subject)
			return err == nil
		}, 10*time.Second, 50*time.Millisecond, subject)
		var env eventbus.Envelope
		require.NoError(t, json.Unmarshal(msg.Data, &env))
		return env
	}
	validate := func(t *testing.T, token string) (validated, int) {
		t.Helper()
		var v validated
		code := call(t, srv, http.MethodPost, "/internal/validate-token", map[string]string{"token": token}, nil, &v)
		return v, code
	}

	var visitor api.Session
	corr := "0192f3a4-5b6c-7d8e-9f01-23456789abcd"
	t.Run("visitor is created, published and valid", func(t *testing.T) {
		hdr := as("shop-a", "", "")
		hdr[authctx.HeaderCorrelationID] = corr
		require.Equal(t, http.StatusCreated, call(t, srv, http.MethodPost, "/visitor", nil, hdr, &visitor))
		assert.Equal(t, "shop-a", visitor.TenantID)
		assert.WithinDuration(t, time.Now().Add(cfg.JWT.VisitorTokenTTL), visitor.ExpiresAt, time.Minute)

		env := lastEvent(t, "yc.shop-a.identity.visitor.created.v1")
		assert.Equal(t, "shop-a", env.TenantID)
		assert.Equal(t, visitor.UserID.String(), env.Actor.ID)
		assert.Equal(t, "visitor", env.Actor.Type)
		assert.NotEmpty(t, env.Actor.AccessTokenJTI)
		assert.Equal(t, corr, env.CorrelationID, "event carries the request's correlation ID")
		assert.Equal(t, "yc-identity", env.SourceService)
		var payload events.IdentityVisitorCreatedV1
		require.NoError(t, json.Unmarshal(env.Payload, &payload))
		assert.Equal(t, visitor.UserID.String(), payload.UserID)

		v, code := validate(t, visitor.AccessToken)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, validated{TenantID: "shop-a", ActorID: visitor.UserID.String(), ActorType: "visitor"}, v)
	})
	visitorID := visitor.UserID.String()

	t.Run("me returns the visitor; other tenants cannot see it", func(t *testing.T) {
		var me api.User
		require.Equal(t, http.StatusOK, call(t, srv, http.MethodGet, "/me", nil, as("shop-a", visitorID, "visitor"), &me))
		assert.Equal(t, visitorID, me.UserID.String())
		assert.Equal(t, api.UserStatus("active"), me.Status)
		assert.Equal(t, http.StatusNotFound, call(t, srv, http.MethodGet, "/me", nil, as("shop-b", visitorID, "visitor"), nil))
	})

	t.Run("platform tenant cannot create visitors", func(t *testing.T) {
		assert.Equal(t, http.StatusBadRequest, call(t, srv, http.MethodPost, "/visitor", nil, as("_platform", "", ""), nil))
	})

	var customer api.Session
	t.Run("a visitor registers in place as a customer", func(t *testing.T) {
		body := map[string]string{"email": "Ann@Example.com", "password": "correct horse battery", "display_name": "Ann"}
		require.Equal(t, http.StatusCreated,
			call(t, srv, http.MethodPost, "/register", body, as("shop-a", visitorID, "visitor"), &customer))
		assert.Equal(t, visitorID, customer.UserID.String(), "the visitor keeps its user ID")
		assert.Equal(t, api.Role("customer"), customer.Role)

		env := lastEvent(t, "yc.shop-a.identity.user.registered.v1")
		var payload events.IdentityUserRegisteredV1
		require.NoError(t, json.Unmarshal(env.Payload, &payload))
		assert.Equal(t, visitorID, payload.UserID)
		assert.Equal(t, "ann@example.com", payload.Email)
		assert.Equal(t, "customer", env.Actor.Type)

		_, code := validate(t, visitor.AccessToken)
		assert.Equal(t, http.StatusUnauthorized, code, "the visitor token stops validating")
		v, code := validate(t, customer.AccessToken)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, "customer", v.ActorType)
	})

	t.Run("an email registers once per tenant", func(t *testing.T) {
		body := map[string]string{"email": "ann@example.com", "password": "another password"}
		assert.Equal(t, http.StatusConflict, call(t, srv, http.MethodPost, "/register", body, as("shop-a", "", ""), nil))
		var other api.Session
		require.Equal(t, http.StatusCreated, call(t, srv, http.MethodPost, "/register", body, as("shop-b", "", ""), &other))
		assert.Equal(t, "shop-b", other.TenantID)
		assert.Equal(t, http.StatusConflict,
			call(t, srv, http.MethodPost, "/register", body, as("shop-a", visitorID, "customer"), nil), "already registered")
	})

	t.Run("login checks the password and reports the previous visitor", func(t *testing.T) {
		var fresh api.Session
		require.Equal(t, http.StatusCreated, call(t, srv, http.MethodPost, "/visitor", nil, as("shop-a", "", ""), &fresh))

		var sess api.Session
		body := map[string]string{"email": "ANN@example.com", "password": "correct horse battery"}
		require.Equal(t, http.StatusOK,
			call(t, srv, http.MethodPost, "/login", body, as("shop-a", fresh.UserID.String(), "visitor"), &sess))
		assert.Equal(t, visitorID, sess.UserID.String())
		assert.Equal(t, api.Role("customer"), sess.Role)

		env := lastEvent(t, "yc.shop-a.identity.user.logged_in.v1")
		var payload events.IdentityUserLoggedInV1
		require.NoError(t, json.Unmarshal(env.Payload, &payload))
		require.NotNil(t, payload.PreviousVisitorID)
		assert.Equal(t, fresh.UserID.String(), *payload.PreviousVisitorID)

		wrong := map[string]string{"email": "ann@example.com", "password": "wrong password!"}
		assert.Equal(t, http.StatusUnauthorized, call(t, srv, http.MethodPost, "/login", wrong, as("shop-a", "", ""), nil))
		unknown := map[string]string{"email": "nobody@example.com", "password": "correct horse battery"}
		assert.Equal(t, http.StatusUnauthorized, call(t, srv, http.MethodPost, "/login", unknown, as("shop-a", "", ""), nil))
		assert.Equal(t, http.StatusUnauthorized, call(t, srv, http.MethodPost, "/login", body, as("shop-c", "", ""), nil),
			"emails are scoped to a tenant")
	})

	var sysadmin api.Session
	t.Run("bootstrap admin is idempotent and logs in on _platform", func(t *testing.T) {
		created, err := app.BootstrapAdmin(ctx, cfg, "root@yc.dev", "platform password", "Root")
		require.NoError(t, err)
		assert.True(t, created)
		created, err = app.BootstrapAdmin(ctx, cfg, "ROOT@yc.dev", "something else", "")
		require.NoError(t, err)
		assert.False(t, created)

		body := map[string]string{"email": "root@yc.dev", "password": "platform password"}
		require.Equal(t, http.StatusOK, call(t, srv, http.MethodPost, "/login", body, as("_platform", "", ""), &sysadmin))
		assert.Equal(t, api.Role("system_admin"), sysadmin.Role)
		assert.Equal(t, "_platform", sysadmin.TenantID)
		v, code := validate(t, sysadmin.AccessToken)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, "system_admin", v.ActorType)

		assert.Equal(t, http.StatusUnauthorized, call(t, srv, http.MethodPost, "/login", body, as("shop-a", "", ""), nil),
			"system admins log in on the platform host only")
	})
	root := as("_platform", sysadmin.UserID.String(), "system_admin")

	t.Run("system admin promotes a customer to shop admin", func(t *testing.T) {
		var list api.UserList
		require.Equal(t, http.StatusOK, call(t, srv, http.MethodGet,
			"/admin/users?tenant_id=shop-a&email=ann@example.com", nil, root, &list))
		require.Len(t, list.Items, 1)
		assert.Equal(t, visitorID, list.Items[0].UserID.String())

		var promoted api.User
		require.Equal(t, http.StatusOK, call(t, srv, http.MethodPost, "/admin/users/"+visitorID+"/promote",
			map[string]string{"role": "shop_admin"}, root, &promoted))
		assert.Equal(t, api.Role("shop_admin"), promoted.Role)

		env := lastEvent(t, "yc.shop-a.identity.user.promoted.v1")
		assert.Equal(t, "system_admin", env.Actor.Type)
		var payload events.IdentityUserPromotedV1
		require.NoError(t, json.Unmarshal(env.Payload, &payload))
		assert.Equal(t, events.IdentityUserPromotedV1{UserID: visitorID, FromRole: "customer", ToRole: "shop_admin", PromotedAt: payload.PromotedAt}, payload)

		_, code := validate(t, customer.AccessToken)
		assert.Equal(t, http.StatusUnauthorized, code, "the customer token stops validating")

		assert.Equal(t, http.StatusConflict, call(t, srv, http.MethodPost, "/admin/users/"+visitorID+"/promote",
			map[string]string{"role": "shop_admin"}, root, nil), "already shop admin")
		assert.Equal(t, http.StatusForbidden, call(t, srv, http.MethodPost, "/admin/users/"+visitorID+"/promote",
			map[string]string{"role": "shop_admin"}, as("shop-a", visitorID, "shop_admin"), nil), "shop admins cannot promote")
	})

	t.Run("shop admins list only their own tenant", func(t *testing.T) {
		shopAdmin := as("shop-a", visitorID, "shop_admin")
		var list api.UserList
		require.Equal(t, http.StatusOK, call(t, srv, http.MethodGet, "/admin/users?page_size=1", nil, shopAdmin, &list))
		require.Len(t, list.Items, 1)
		require.NotEmpty(t, list.Page.NextCursor)
		for _, u := range list.Items {
			assert.Equal(t, "shop-a", u.TenantID)
		}
		var next api.UserList
		require.Equal(t, http.StatusOK, call(t, srv, http.MethodGet,
			"/admin/users?page_size=100&cursor="+list.Page.NextCursor, nil, shopAdmin, &next))
		for _, u := range next.Items {
			assert.Equal(t, "shop-a", u.TenantID)
			assert.NotEqual(t, list.Items[0].UserID, u.UserID, "pages do not overlap")
		}
		assert.Equal(t, http.StatusForbidden, call(t, srv, http.MethodGet, "/admin/users?tenant_id=shop-b", nil, shopAdmin, nil))
		assert.Equal(t, http.StatusForbidden, call(t, srv, http.MethodGet, "/admin/users", nil, as("shop-a", visitorID, "customer"), nil))
	})

	t.Run("suspended user's token is rejected", func(t *testing.T) {
		var sess api.Session
		body := map[string]string{"email": "ann@example.com", "password": "correct horse battery"}
		require.Equal(t, http.StatusOK, call(t, srv, http.MethodPost, "/login", body, as("shop-a", "", ""), &sess))
		require.NoError(t, dbkit.WithTenantTx(ctx, db, "shop-a", func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE users SET status = 'suspended' WHERE id = $1`, visitorID)
			return err
		}))
		_, code := validate(t, sess.AccessToken)
		assert.Equal(t, http.StatusUnauthorized, code)
		assert.Equal(t, http.StatusUnauthorized, call(t, srv, http.MethodPost, "/login", body, as("shop-a", "", ""), nil))
	})

	t.Run("readyz reports dependencies", func(t *testing.T) {
		var r httpkit.Readiness
		require.Equal(t, http.StatusOK, call(t, srv, http.MethodGet, "/readyz", nil, nil, &r))
		assert.Equal(t, map[string]string{"postgres": "ok", "nats": "ok"}, r.Checks)
	})
}
