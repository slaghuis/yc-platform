// Package oauth talks to the OAuth providers customers sign in with: the
// authorisation code flow with PKCE, then the provider's profile endpoint.
package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/oauth2"

	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/domain"
)

// Provider names.
const (
	Google   = "google"
	Facebook = "facebook"
)

// ErrExchange means the provider rejected the code or returned no usable
// profile. The cause is wrapped for logs.
var ErrExchange = errors.New("oauth: exchange failed")

// Provider is one OAuth provider.
type Provider interface {
	// AuthCodeURL is where to send the browser.
	AuthCodeURL(state, verifier, redirectURL, loginHint string) string
	// Exchange trades the code for the signed-in account's profile.
	Exchange(ctx context.Context, code, verifier, redirectURL string) (domain.OAuthProfile, error)
}

// ProviderConfig is one provider's block under `oauth:`. The endpoint URLs
// default to the provider's; tests point them elsewhere.
type ProviderConfig struct {
	ClientID     string `koanf:"client_id"`
	ClientSecret string `koanf:"client_secret"`
	// RedirectURL is the shop's callback page; "{host}" is replaced with the
	// host the request came in on.
	RedirectURL string `koanf:"redirect_url"`
	// Fake replaces the provider with one that signs in whatever email the
	// client passes as login_hint. Development only.
	Fake bool `koanf:"fake"`

	AuthURL     string `koanf:"auth_url"`
	TokenURL    string `koanf:"token_url"`
	UserInfoURL string `koanf:"userinfo_url"`
}

// NewVerifier returns a PKCE code verifier.
func NewVerifier() string { return oauth2.GenerateVerifier() }

// Enabled reports whether the provider is configured.
func (c ProviderConfig) Enabled() bool {
	return c.Fake || (c.ClientID != "" && c.ClientSecret != "")
}

// New returns the provider name configured by c, which must be Enabled.
func New(name string, c ProviderConfig, timeout time.Duration) (Provider, error) {
	if c.Fake {
		return fake{}, nil
	}
	client := httpkit.NewClient(httpkit.ClientOptions{Timeout: timeout})
	switch name {
	case Google:
		return &standard{
			cfg: oauth2.Config{
				ClientID: c.ClientID, ClientSecret: c.ClientSecret,
				Endpoint: oauth2.Endpoint{
					AuthURL:   or(c.AuthURL, "https://accounts.google.com/o/oauth2/v2/auth"),
					TokenURL:  or(c.TokenURL, "https://oauth2.googleapis.com/token"),
					AuthStyle: oauth2.AuthStyleInParams,
				},
				Scopes: []string{"openid", "email", "profile"},
			},
			userInfo: or(c.UserInfoURL, "https://openidconnect.googleapis.com/v1/userinfo"),
			client:   client,
			parse:    parseGoogle,
		}, nil
	case Facebook:
		return &standard{
			cfg: oauth2.Config{
				ClientID: c.ClientID, ClientSecret: c.ClientSecret,
				Endpoint: oauth2.Endpoint{
					AuthURL:   or(c.AuthURL, "https://www.facebook.com/v19.0/dialog/oauth"),
					TokenURL:  or(c.TokenURL, "https://graph.facebook.com/v19.0/oauth/access_token"),
					AuthStyle: oauth2.AuthStyleInParams,
				},
				Scopes: []string{"email", "public_profile"},
			},
			userInfo: or(c.UserInfoURL, "https://graph.facebook.com/me?fields=id,name,email,picture.type(large)"),
			client:   client,
			parse:    parseFacebook,
		}, nil
	default:
		return nil, fmt.Errorf("oauth: unknown provider %q", name)
	}
}

type standard struct {
	cfg      oauth2.Config
	userInfo string
	client   *http.Client
	parse    func([]byte) (domain.OAuthProfile, error)
}

func (p *standard) withRedirect(redirectURL string) *oauth2.Config {
	c := p.cfg
	c.RedirectURL = redirectURL
	return &c
}

func (p *standard) AuthCodeURL(state, verifier, redirectURL, loginHint string) string {
	opts := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(verifier)}
	if loginHint != "" {
		opts = append(opts, oauth2.SetAuthURLParam("login_hint", loginHint))
	}
	return p.withRedirect(redirectURL).AuthCodeURL(state, opts...)
}

func (p *standard) Exchange(ctx context.Context, code, verifier, redirectURL string) (domain.OAuthProfile, error) {
	ctx = context.WithValue(ctx, oauth2.HTTPClient, p.client)
	tok, err := p.withRedirect(redirectURL).Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return domain.OAuthProfile{}, fmt.Errorf("%w: token: %w", ErrExchange, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.userInfo, http.NoBody)
	if err != nil {
		return domain.OAuthProfile{}, fmt.Errorf("oauth: userinfo request: %w", err)
	}
	tok.SetAuthHeader(req)
	resp, err := p.client.Do(req)
	if err != nil {
		return domain.OAuthProfile{}, fmt.Errorf("%w: userinfo: %w", ErrExchange, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return domain.OAuthProfile{}, fmt.Errorf("%w: userinfo status %d", ErrExchange, resp.StatusCode)
	}
	var body json.RawMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body); err != nil {
		return domain.OAuthProfile{}, fmt.Errorf("%w: userinfo body: %w", ErrExchange, err)
	}
	profile, err := p.parse(body)
	if err != nil {
		return domain.OAuthProfile{}, fmt.Errorf("%w: %w", ErrExchange, err)
	}
	return profile, nil
}

func parseGoogle(b []byte) (domain.OAuthProfile, error) {
	var v struct {
		Sub           string `json:"sub"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
		Picture       string `json:"picture"`
		PhoneNumber   string `json:"phone_number"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return domain.OAuthProfile{}, err
	}
	return profile(v.Sub, v.Email, v.EmailVerified, v.Name, v.PhoneNumber, v.Picture)
}

// parseFacebook never reports the email verified: Facebook does not say, so
// a Facebook account is never linked to an existing account by email.
func parseFacebook(b []byte) (domain.OAuthProfile, error) {
	var v struct {
		ID      string `json:"id"`
		Email   string `json:"email"`
		Name    string `json:"name"`
		Picture struct {
			Data struct {
				URL string `json:"url"`
			} `json:"data"`
		} `json:"picture"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return domain.OAuthProfile{}, err
	}
	return profile(v.ID, v.Email, false, v.Name, "", v.Picture.Data.URL)
}

func profile(sub, email string, verified bool, name, phone, imageURL string) (domain.OAuthProfile, error) {
	if sub == "" {
		return domain.OAuthProfile{}, errors.New("profile has no subject")
	}
	norm, ok := domain.NormaliseEmail(email)
	if !ok {
		norm, verified = "", false
	}
	name, _ = domain.NormaliseDisplayName(name)
	if len([]rune(name)) > 100 {
		name = string([]rune(name)[:100])
	}
	out := domain.OAuthProfile{Subject: sub, Email: norm, EmailVerified: verified, Name: name}
	if p, ok := domain.NormalisePhoneE164(phone); ok {
		out.PhoneE164 = p
	}
	if img, ok := domain.NormaliseProfileImageURL(imageURL); ok {
		out.ProfileImage = img
	}
	return out, nil
}

// fake signs in the login_hint email without a provider. Its "code" is the
// profile itself, so anyone can claim any email: development only.
type fake struct{}

type fakeCode struct {
	Sub     string `json:"sub"`
	Email   string `json:"email"`
	Phone   string `json:"phone,omitempty"`
	Picture string `json:"picture,omitempty"`
}

func (fake) AuthCodeURL(state, _, redirectURL, loginHint string) string {
	b, _ := json.Marshal(fakeCode{Sub: "fake-" + loginHint, Email: loginHint})
	q := url.Values{"state": {state}, "code": {base64.RawURLEncoding.EncodeToString(b)}}
	return redirectURL + "?" + q.Encode()
}

func (fake) Exchange(_ context.Context, code, _, _ string) (domain.OAuthProfile, error) {
	b, err := base64.RawURLEncoding.DecodeString(code)
	if err != nil {
		return domain.OAuthProfile{}, fmt.Errorf("%w: fake code: %w", ErrExchange, err)
	}
	var c fakeCode
	if err := json.Unmarshal(b, &c); err != nil {
		return domain.OAuthProfile{}, fmt.Errorf("%w: fake code: %w", ErrExchange, err)
	}
	p, err := profile(c.Sub, c.Email, true, "", c.Phone, c.Picture)
	if err != nil {
		return domain.OAuthProfile{}, fmt.Errorf("%w: %w", ErrExchange, err)
	}
	return p, nil
}

func or(v, def string) string {
	if v != "" {
		return v
	}
	return def
}
