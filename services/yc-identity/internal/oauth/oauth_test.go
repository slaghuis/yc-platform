package oauth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/slaghuis/yc-platform/services/yc-identity/internal/oauth"
)

// fakeProvider is a provider's token and profile endpoints.
func fakeProvider(t *testing.T, profile map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			require.NoError(t, r.ParseForm())
			if r.Form.Get("code") != "good-code" || r.Form.Get("code_verifier") == "" ||
				r.Form.Get("redirect_uri") != "https://shop-a.test/cb" || r.Form.Get("client_secret") != "secret" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"at","token_type":"Bearer","expires_in":3600}`))
		case "/userinfo":
			if r.Header.Get("Authorization") != "Bearer at" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(profile)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func cfg(srv *httptest.Server) oauth.ProviderConfig {
	return oauth.ProviderConfig{
		ClientID: "client", ClientSecret: "secret",
		AuthURL: srv.URL + "/auth", TokenURL: srv.URL + "/token", UserInfoURL: srv.URL + "/userinfo",
	}
}

func TestGoogle(t *testing.T) {
	t.Parallel()
	srv := fakeProvider(t, map[string]any{
		"sub": "g-1", "email": "Ann@Example.com", "email_verified": true, "name": " Ann ",
	})
	p, err := oauth.New(oauth.Google, cfg(srv), 5*time.Second)
	require.NoError(t, err)

	verifier := oauth.NewVerifier()
	u, err := url.Parse(p.AuthCodeURL("st", verifier, "https://shop-a.test/cb", "ann@example.com"))
	require.NoError(t, err)
	q := u.Query()
	assert.Equal(t, "st", q.Get("state"))
	assert.Equal(t, "S256", q.Get("code_challenge_method"))
	assert.NotEmpty(t, q.Get("code_challenge"))
	assert.Equal(t, "https://shop-a.test/cb", q.Get("redirect_uri"))
	assert.Equal(t, "ann@example.com", q.Get("login_hint"))
	assert.Contains(t, q.Get("scope"), "email")

	profile, err := p.Exchange(context.Background(), "good-code", verifier, "https://shop-a.test/cb")
	require.NoError(t, err)
	assert.Equal(t, "g-1", profile.Subject)
	assert.Equal(t, "ann@example.com", profile.Email)
	assert.True(t, profile.EmailVerified)
	assert.Equal(t, "Ann", profile.Name)

	_, err = p.Exchange(context.Background(), "bad-code", verifier, "https://shop-a.test/cb")
	assert.ErrorIs(t, err, oauth.ErrExchange)
}

func TestFacebookNeverVerifiesEmail(t *testing.T) {
	t.Parallel()
	srv := fakeProvider(t, map[string]any{"id": "fb-1", "email": "bob@example.com", "name": "Bob"})
	p, err := oauth.New(oauth.Facebook, cfg(srv), 5*time.Second)
	require.NoError(t, err)
	profile, err := p.Exchange(context.Background(), "good-code", oauth.NewVerifier(), "https://shop-a.test/cb")
	require.NoError(t, err)
	assert.Equal(t, "fb-1", profile.Subject)
	assert.Equal(t, "bob@example.com", profile.Email)
	assert.False(t, profile.EmailVerified)
}

func TestProfileWithoutSubjectFails(t *testing.T) {
	t.Parallel()
	srv := fakeProvider(t, map[string]any{"email": "x@example.com"})
	p, err := oauth.New(oauth.Google, cfg(srv), 5*time.Second)
	require.NoError(t, err)
	_, err = p.Exchange(context.Background(), "good-code", oauth.NewVerifier(), "https://shop-a.test/cb")
	assert.ErrorIs(t, err, oauth.ErrExchange)
}

func TestFake(t *testing.T) {
	t.Parallel()
	p, err := oauth.New(oauth.Google, oauth.ProviderConfig{Fake: true}, time.Second)
	require.NoError(t, err)
	u, err := url.Parse(p.AuthCodeURL("st", "v", "http://shop-a.localhost/cb", "Cat@Example.com"))
	require.NoError(t, err)
	assert.Equal(t, "st", u.Query().Get("state"))
	profile, err := p.Exchange(context.Background(), u.Query().Get("code"), "v", "")
	require.NoError(t, err)
	assert.Equal(t, "cat@example.com", profile.Email)
	assert.True(t, profile.EmailVerified)

	_, err = p.Exchange(context.Background(), "%%%", "v", "")
	assert.ErrorIs(t, err, oauth.ErrExchange)
}
