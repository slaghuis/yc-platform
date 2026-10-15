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
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
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

// facebookStub serves Facebook's token and profile endpoints for an account
// whose email belongs to an existing user.
func facebookStub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_ = r.ParseForm()
			if r.Form.Get("code") != "good-code" || r.Form.Get("code_verifier") == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"fb-at","token_type":"Bearer","expires_in":3600}`))
		case "/me":
			_, _ = w.Write([]byte(`{"id":"fb-1","email":"bob@example.com","name":"Bob"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// sent is a send request yc-message received through the gateway.
type sent struct {
	Tenant string
	Body   map[string]any
}

// messageStub is yc-gateway in front of yc-message: it issues yc-identity's
// service token and records /internal/send calls.
func messageStub(t *testing.T) (*httptest.Server, func() []sent) {
	t.Helper()
	var (
		mu    sync.Mutex
		sends []sent
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/internal/service-token":
			_, _ = w.Write([]byte(`{"access_token":"svc-yc-identity","expires_at":"2099-01-01T00:00:00Z"}`))
		case "/internal/send":
			if r.Header.Get(authctx.HeaderAuthorization) != "Bearer svc-yc-identity" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			sends = append(sends, sent{Tenant: r.Header.Get(authctx.HeaderTenantID), Body: body})
			mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"message_id":"m1","status":"queued"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []sent {
		mu.Lock()
		defer mu.Unlock()
		return append([]sent(nil), sends...)
	}
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
	t.Setenv("YC_IDENTITY_LOG_RESET_LINKS", "true")
	const cartSecret = "cart-secret-0123456789-0123456789-abcdef"
	t.Setenv("YC_CART_SERVICE_SECRET", cartSecret)
	t.Setenv("YC_IDENTITY__OAUTH__GOOGLE__FAKE", "true")
	fb := facebookStub(t)
	t.Setenv("YC_IDENTITY_FACEBOOK_CLIENT_ID", "fb-client")
	t.Setenv("YC_IDENTITY_FACEBOOK_CLIENT_SECRET", "fb-secret")
	t.Setenv("YC_IDENTITY__OAUTH__FACEBOOK__AUTH_URL", fb.URL+"/auth")
	t.Setenv("YC_IDENTITY__OAUTH__FACEBOOK__TOKEN_URL", fb.URL+"/token")
	t.Setenv("YC_IDENTITY__OAUTH__FACEBOOK__USERINFO_URL", fb.URL+"/me")
	gateway, sends := messageStub(t)
	t.Setenv("YC_GATEWAY_INTERNAL_URL", gateway.URL)
	t.Setenv("YC_IDENTITY_SERVICE_SECRET", "identity-secret-0123456789-0123456789")
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
	// eventWith waits until the last event on subject has a payload
	// containing want, since an earlier event may still be the last one.
	eventWith := func(t *testing.T, subject, want string) eventbus.Envelope {
		t.Helper()
		var env eventbus.Envelope
		require.Eventually(t, func() bool {
			env = lastEvent(t, subject)
			return bytes.Contains(env.Payload, []byte(want))
		}, 10*time.Second, 50*time.Millisecond, subject+" with "+want)
		return env
	}
	internal := map[string]string{authctx.HeaderCallerService: "yc-gateway"}
	validate := func(t *testing.T, token string) (validated, int) {
		t.Helper()
		var v validated
		code := call(t, srv, http.MethodPost, "/internal/validate-token", map[string]string{"token": token}, internal, &v)
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
		assert.Nil(t, visitor.RefreshToken, "visitors get no refresh token")

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

	t.Run("service tokens", func(t *testing.T) {
		var tok api.ServiceToken
		require.Equal(t, http.StatusOK, call(t, srv, http.MethodPost, "/internal/service-token",
			map[string]string{"client_id": "yc-cart", "client_secret": cartSecret}, internal, &tok))
		v, code := validate(t, tok.AccessToken)
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, validated{TenantID: "_platform", ActorID: "yc-cart", ActorType: "service"}, v)

		assert.Equal(t, http.StatusUnauthorized, call(t, srv, http.MethodPost, "/internal/service-token",
			map[string]string{"client_id": "yc-cart", "client_secret": "wrong"}, internal, nil))
		assert.Equal(t, http.StatusUnauthorized, call(t, srv, http.MethodPost, "/internal/service-token",
			map[string]string{"client_id": "yc-nope", "client_secret": cartSecret}, internal, nil))
		assert.Equal(t, http.StatusUnauthorized, call(t, srv, http.MethodPost, "/internal/service-token",
			map[string]string{"client_id": "yc-cart", "client_secret": cartSecret}, nil, nil), "needs the gateway")
	})

	bob := map[string]string{"email": "bob@example.com", "password": "bobs first password"}
	var bobID string
	t.Run("refresh rotates, reuse revokes the login, logout revokes", func(t *testing.T) {
		var reg api.Session
		require.Equal(t, http.StatusCreated, call(t, srv, http.MethodPost, "/register", bob, as("shop-c", "", ""), &reg))
		bobID = reg.UserID.String()
		require.NotNil(t, reg.RefreshToken)
		assert.WithinDuration(t, time.Now().Add(cfg.JWT.RefreshTokenTTL), *reg.RefreshExpiresAt, time.Minute)

		var next api.Session
		require.Equal(t, http.StatusOK, call(t, srv, http.MethodPost, "/refresh",
			map[string]string{"refresh_token": *reg.RefreshToken}, as("shop-c", "", ""), &next))
		require.NotNil(t, next.RefreshToken)
		assert.NotEqual(t, *reg.RefreshToken, *next.RefreshToken)
		assert.Equal(t, bobID, next.UserID.String())
		_, code := validate(t, next.AccessToken)
		assert.Equal(t, http.StatusOK, code)

		assert.Equal(t, http.StatusUnauthorized, call(t, srv, http.MethodPost, "/refresh",
			map[string]string{"refresh_token": *next.RefreshToken}, as("shop-a", "", ""), nil), "other tenant")
		assert.Equal(t, http.StatusUnauthorized, call(t, srv, http.MethodPost, "/refresh",
			map[string]string{"refresh_token": *reg.RefreshToken}, as("shop-c", "", ""), nil), "reused token")
		assert.Equal(t, http.StatusUnauthorized, call(t, srv, http.MethodPost, "/refresh",
			map[string]string{"refresh_token": *next.RefreshToken}, as("shop-c", "", ""), nil), "reuse revoked the login")

		var s2 api.Session
		require.Equal(t, http.StatusOK, call(t, srv, http.MethodPost, "/login", bob, as("shop-c", "", ""), &s2))
		hdr := as("shop-c", bobID, "customer")
		hdr["Authorization"] = "Bearer " + s2.AccessToken
		require.Equal(t, http.StatusNoContent, call(t, srv, http.MethodPost, "/logout",
			map[string]string{"refresh_token": *s2.RefreshToken}, hdr, nil))
		_, code = validate(t, s2.AccessToken)
		assert.Equal(t, http.StatusUnauthorized, code, "logged-out access token")
		assert.Equal(t, http.StatusUnauthorized, call(t, srv, http.MethodPost, "/refresh",
			map[string]string{"refresh_token": *s2.RefreshToken}, as("shop-c", "", ""), nil), "logged-out refresh token")
	})

	t.Run("password reset", func(t *testing.T) {
		require.Equal(t, http.StatusAccepted, call(t, srv, http.MethodPost, "/password/reset-request",
			map[string]string{"email": "nobody@example.com"}, as("shop-c", "", ""), nil), "unknown email looks the same")
		require.Equal(t, http.StatusAccepted, call(t, srv, http.MethodPost, "/password/reset-request",
			map[string]string{"email": "Bob@Example.com"}, as("shop-c", "", ""), nil))

		env := lastEvent(t, "yc.shop-c.identity.user.password_reset_requested.v1")
		var req events.IdentityUserPasswordResetRequestedV1
		require.NoError(t, json.Unmarshal(env.Payload, &req))
		assert.Equal(t, bobID, req.UserID)
		assert.Equal(t, "bob@example.com", req.Email)

		m := regexp.MustCompile(`"reset_token":"([^"]+)"`).FindStringSubmatch(logs.String())
		require.Len(t, m, 2, "log_reset_links logs the token")
		token := m[1]
		assert.NotContains(t, string(env.Payload), token, "the event carries no token")

		got := sends()
		require.Len(t, got, 1, "only the registered email gets a link")
		assert.Equal(t, "shop-c", got[0].Tenant)
		assert.Equal(t, "password_reset", got[0].Body["type"])
		assert.Equal(t, "password-reset:"+req.RequestID, got[0].Body["reference"])
		assert.Equal(t, bobID, got[0].Body["user_id"])
		assert.Equal(t, "email", got[0].Body["channel"])
		assert.Equal(t, "bob@example.com", got[0].Body["address"])
		host := strings.TrimPrefix(srv.URL, "http://")
		msgCtx, _ := got[0].Body["context"].(map[string]any)
		assert.Equal(t, "https://"+host+"/account/reset-password?token="+token, msgCtx["reset_url"])
		assert.Equal(t, req.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC"), msgCtx["expires_at"])

		var sess api.Session
		require.Equal(t, http.StatusOK, call(t, srv, http.MethodPost, "/login", bob, as("shop-c", "", ""), &sess))

		assert.Equal(t, http.StatusBadRequest, call(t, srv, http.MethodPost, "/password/reset-confirm",
			map[string]string{"token": token, "password": "a brand new password"}, as("shop-a", "", ""), nil), "other tenant")
		require.Equal(t, http.StatusNoContent, call(t, srv, http.MethodPost, "/password/reset-confirm",
			map[string]string{"token": token, "password": "a brand new password"}, as("shop-c", "", ""), nil))
		assert.Equal(t, http.StatusBadRequest, call(t, srv, http.MethodPost, "/password/reset-confirm",
			map[string]string{"token": token, "password": "yet another password"}, as("shop-c", "", ""), nil), "single use")

		assert.Equal(t, http.StatusUnauthorized, call(t, srv, http.MethodPost, "/login", bob, as("shop-c", "", ""), nil))
		bob["password"] = "a brand new password"
		require.Equal(t, http.StatusOK, call(t, srv, http.MethodPost, "/login", bob, as("shop-c", "", ""), nil))
		assert.Equal(t, http.StatusUnauthorized, call(t, srv, http.MethodPost, "/refresh",
			map[string]string{"refresh_token": *sess.RefreshToken}, as("shop-c", "", ""), nil), "refresh tokens revoked")

		env = lastEvent(t, "yc.shop-c.identity.user.password_changed.v1")
		assert.Equal(t, bobID, env.Actor.ID)
	})

	oauthFlow := func(t *testing.T, provider, visitorID, hint string) (code, state string) {
		t.Helper()
		hdr := as("shop-c", visitorID, "visitor")
		hdr["X-Forwarded-Host"] = "shop-c.test"
		var start api.OAuthStart
		require.Equal(t, http.StatusOK, call(t, srv, http.MethodPost, "/oauth/"+provider+"/start",
			map[string]string{"login_hint": hint}, hdr, &start))
		u, err := url.Parse(start.AuthorizationURL)
		require.NoError(t, err)
		q := u.Query()
		if provider == "google" {
			assert.Equal(t, "https://shop-c.test/auth/callback/google", u.Scheme+"://"+u.Host+u.Path, "fake redirects straight back")
			return q.Get("code"), q.Get("state")
		}
		assert.Equal(t, "https://shop-c.test/auth/callback/facebook", q.Get("redirect_uri"))
		assert.NotEmpty(t, q.Get("code_challenge"))
		return "good-code", q.Get("state")
	}
	newVisitor := func(t *testing.T) string {
		t.Helper()
		var v api.Session
		require.Equal(t, http.StatusCreated, call(t, srv, http.MethodPost, "/visitor", nil, as("shop-c", "", ""), &v))
		return v.UserID.String()
	}

	t.Run("oauth registers the visitor in place, then logs in", func(t *testing.T) {
		v1 := newVisitor(t)
		code, state := oauthFlow(t, "google", v1, "cat@example.com")
		var sess api.Session
		require.Equal(t, http.StatusOK, call(t, srv, http.MethodPost, "/oauth/google/callback",
			map[string]string{"code": code, "state": state}, as("shop-c", v1, "visitor"), &sess))
		assert.Equal(t, v1, sess.UserID.String(), "the visitor keeps its user ID")
		assert.Equal(t, api.Role("customer"), sess.Role)
		require.NotNil(t, sess.RefreshToken)
		var reg events.IdentityUserRegisteredV1
		require.NoError(t, json.Unmarshal(eventWith(t, "yc.shop-c.identity.user.registered.v1", v1).Payload, &reg))
		assert.Equal(t, "google", reg.Method)
		assert.Equal(t, "cat@example.com", reg.Email)
		eventWith(t, "yc.shop-c.identity.oauth_linked.v1", v1)

		assert.Equal(t, http.StatusUnauthorized, call(t, srv, http.MethodPost, "/oauth/google/callback",
			map[string]string{"code": code, "state": state}, as("shop-c", v1, "visitor"), nil), "state is single use")

		v2 := newVisitor(t)
		code, state = oauthFlow(t, "google", v2, "cat@example.com")
		require.Equal(t, http.StatusOK, call(t, srv, http.MethodPost, "/oauth/google/callback",
			map[string]string{"code": code, "state": state}, as("shop-c", v2, "visitor"), &sess))
		assert.Equal(t, v1, sess.UserID.String(), "the linked account logs in")
		var in events.IdentityUserLoggedInV1
		require.NoError(t, json.Unmarshal(eventWith(t, "yc.shop-c.identity.user.logged_in.v1", v2).Payload, &in))
		assert.Equal(t, "google", in.Method)
		require.NotNil(t, in.PreviousVisitorID)
		assert.Equal(t, v2, *in.PreviousVisitorID, "the second visitor's cart merges")
	})

	t.Run("oauth links a verified email to an existing account", func(t *testing.T) {
		v := newVisitor(t)
		code, state := oauthFlow(t, "google", v, "bob@example.com")
		var sess api.Session
		require.Equal(t, http.StatusOK, call(t, srv, http.MethodPost, "/oauth/google/callback",
			map[string]string{"code": code, "state": state}, as("shop-c", v, "visitor"), &sess))
		assert.Equal(t, bobID, sess.UserID.String())
		eventWith(t, "yc.shop-c.identity.oauth_linked.v1", bobID)
	})

	t.Run("facebook never links an existing account by email", func(t *testing.T) {
		v := newVisitor(t)
		code, state := oauthFlow(t, "facebook", v, "")
		assert.Equal(t, http.StatusConflict, call(t, srv, http.MethodPost, "/oauth/facebook/callback",
			map[string]string{"code": code, "state": state}, as("shop-c", v, "visitor"), nil))
	})

	t.Run("oauth on the platform host is refused", func(t *testing.T) {
		assert.Equal(t, http.StatusBadRequest, call(t, srv, http.MethodPost, "/oauth/google/start",
			nil, as("_platform", "", ""), nil))
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
