package app

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	config "github.com/slaghuis/yc-platform/libs/yc-config"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	defaults "github.com/slaghuis/yc-platform/services/yc-identity/config"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/oauth"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/password"
)

// Config is the service configuration (config/config.default.yaml).
type Config struct {
	Service       string                `koanf:"service"`
	Environment   string                `koanf:"environment"`
	HTTP          httpkit.ServerConfig  `koanf:"http"`
	Log           LogConfig             `koanf:"log"`
	Database      dbkit.Config          `koanf:"database"`
	NATS          NATSConfig            `koanf:"nats"`
	Telemetry     TelemetryConfig       `koanf:"telemetry"`
	JWT           JWTConfig             `koanf:"jwt"`
	Password      PasswordConfig        `koanf:"password"`
	Invitations   InvitationsConfig     `koanf:"invitations"`
	PlatformHosts []string              `koanf:"platform_hosts"`
	Gateway       httpkit.GatewayConfig `koanf:"gateway"`
	Idempotency   IdempotencyConfig     `koanf:"idempotency"`
	// ServiceClients maps service name to client secret (ADR-021). Clients
	// with an empty secret are left out.
	ServiceClients map[string]string `koanf:"service_clients"`
	OAuth          OAuthConfig       `koanf:"oauth"`
	// InternalCalls reaches yc-message for reset and invite links. Without
	// a gateway URL links are not sent.
	InternalCalls httpkit.InternalConfig `koanf:"internal_calls"`
}

// OAuthConfig is the `oauth:` block.
type OAuthConfig struct {
	StateTTL time.Duration        `koanf:"state_ttl"`
	Timeout  time.Duration        `koanf:"timeout"`
	Google   oauth.ProviderConfig `koanf:"google"`
	Facebook oauth.ProviderConfig `koanf:"facebook"`
}

// Providers returns the enabled providers' configs by name.
func (c OAuthConfig) Providers() map[string]oauth.ProviderConfig {
	out := map[string]oauth.ProviderConfig{}
	for name, p := range map[string]oauth.ProviderConfig{oauth.Google: c.Google, oauth.Facebook: c.Facebook} {
		if p.Enabled() {
			out[name] = p
		}
	}
	return out
}

// minServiceSecretLen is ADR-021 rule 1.
const minServiceSecretLen = 32

// serviceClients returns the configured clients with a secret.
func (c *Config) serviceClients() map[string]string {
	out := map[string]string{}
	for name, secret := range c.ServiceClients {
		if secret != "" {
			out[name] = secret
		}
	}
	return out
}

// LogConfig is the `log:` block.
type LogConfig struct {
	Level string `koanf:"level"`
}

// NATSConfig is the `nats:` block.
type NATSConfig struct {
	URL string `koanf:"url"`

	// DisableRelay leaves published events in the outbox. Only for the
	// outbox crash drill (scripts/demo-phase1.sh).
	DisableRelay bool `koanf:"disable_relay"`
}

// TelemetryConfig is the `telemetry:` block.
type TelemetryConfig struct {
	OTLPEndpoint string `koanf:"otlp_endpoint"`
}

// JWTConfig is the `jwt:` block.
type JWTConfig struct {
	Issuer          string        `koanf:"issuer"`
	PrivateKeyFile  string        `koanf:"private_key_file"`
	AccessTokenTTL  time.Duration `koanf:"access_token_ttl"`
	VisitorTokenTTL time.Duration `koanf:"visitor_token_ttl"`
	RefreshTokenTTL time.Duration `koanf:"refresh_token_ttl"`
	ServiceTokenTTL time.Duration `koanf:"service_token_ttl"`
}

// PasswordConfig is the `password:` block.
type PasswordConfig struct {
	Argon2   password.Params `koanf:"argon2"`
	ResetTTL time.Duration   `koanf:"reset_ttl"`
	// ResetURL is the shop's reset page emailed to the user; "{host}" is
	// the shop's host and "{token}" the reset token.
	ResetURL string `koanf:"reset_url"`
	// LogResetLinks logs reset tokens. Refused in production.
	LogResetLinks bool `koanf:"log_reset_links"`
	// LoginMaxFailures failed sign-ins for one email in a shop refuse the
	// rest of the LoginFailureWindow; 0 turns the limit off.
	LoginMaxFailures   int           `koanf:"login_max_failures"`
	LoginFailureWindow time.Duration `koanf:"login_failure_window"`
}

// InvitationsConfig is the `invitations:` block (ADR-030).
type InvitationsConfig struct {
	TTL time.Duration `koanf:"ttl"`
	// AcceptURL is the accept-invite page; "{host}" and "{token}".
	AcceptURL string `koanf:"accept_url"`
	// LogInviteLinks logs invite tokens. Refused in production.
	LogInviteLinks bool `koanf:"log_invite_links"`
}

// IdempotencyConfig is the `idempotency:` block.
type IdempotencyConfig struct {
	Retention     time.Duration `koanf:"retention"`
	PurgeInterval time.Duration `koanf:"purge_interval"`
}

// LoadConfig layers the embedded defaults, the optional file named by
// YC_CONFIG_FILE and YC_IDENTITY__* environment overrides.
func LoadConfig() (Config, error) {
	cfg := Config{HTTP: httpkit.DefaultServerConfig(), Database: dbkit.DefaultConfig()}
	err := config.Load(&cfg,
		config.Bytes("config.default.yaml", defaults.Default),
		config.OptionalFile(os.Getenv("YC_CONFIG_FILE")),
		config.Env("YC_IDENTITY"),
	)
	return cfg, err
}

// platformHosts returns the platform hostnames, split, trimmed and lowercased.
func (c *Config) platformHosts() []string {
	var out []string
	for _, item := range c.PlatformHosts {
		for s := range strings.SplitSeq(item, ",") {
			s = strings.ToLower(strings.TrimSpace(s))
			if s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// Validate reports missing required settings.
func (c *Config) Validate() error {
	var errs []error
	if c.Database.URL == "" {
		errs = append(errs, errors.New("database.url is required (YC_IDENTITY_DB_URL)"))
	}
	if c.NATS.URL == "" {
		errs = append(errs, errors.New("nats.url is required (NATS_URL)"))
	}
	if c.JWT.PrivateKeyFile == "" {
		errs = append(errs, errors.New("jwt.private_key_file is required (YC_IDENTITY_JWT_PRIVATE_KEY_FILE)"))
	}
	if c.JWT.VisitorTokenTTL <= 0 || c.JWT.AccessTokenTTL <= 0 || c.JWT.RefreshTokenTTL <= 0 || c.JWT.ServiceTokenTTL <= 0 {
		errs = append(errs, errors.New("jwt token TTLs must be positive"))
	}
	if err := c.Password.Argon2.Validate(); err != nil {
		errs = append(errs, err)
	}
	if c.Password.ResetTTL <= 0 || c.OAuth.StateTTL <= 0 || c.OAuth.Timeout <= 0 {
		errs = append(errs, errors.New("password.reset_ttl, oauth.state_ttl and oauth.timeout must be positive"))
	}
	if c.Invitations.TTL <= 0 {
		errs = append(errs, errors.New("invitations.ttl must be positive"))
	}
	if !strings.Contains(c.Invitations.AcceptURL, "{token}") {
		errs = append(errs, errors.New("invitations.accept_url must contain {token}"))
	}
	if c.Password.LoginMaxFailures < 0 || (c.Password.LoginMaxFailures > 0 && c.Password.LoginFailureWindow <= 0) {
		errs = append(errs, errors.New("password.login_max_failures must be 0 or more, with a positive login_failure_window"))
	}
	if c.Gateway.InternalURL != "" && c.Gateway.AdvertiseURL == "" {
		errs = append(errs, errors.New("gateway.advertise_url is required with gateway.internal_url"))
	}
	if c.Idempotency.Retention < 24*time.Hour {
		errs = append(errs, errors.New("idempotency.retention must be at least 24h"))
	}
	if c.Idempotency.PurgeInterval <= 0 {
		errs = append(errs, errors.New("idempotency.purge_interval must be positive"))
	}
	for name, secret := range c.serviceClients() {
		if len(secret) < minServiceSecretLen {
			errs = append(errs, fmt.Errorf("service_clients.%s: secret must be at least %d characters", name, minServiceSecretLen))
		}
	}
	for name, p := range c.OAuth.Providers() {
		if p.RedirectURL == "" {
			errs = append(errs, fmt.Errorf("oauth.%s.redirect_url is required", name))
		}
		if p.Fake && c.Environment == "production" {
			errs = append(errs, fmt.Errorf("oauth.%s.fake is not allowed in production", name))
		}
	}
	if c.InternalCalls.GatewayURL != "" {
		if c.InternalCalls.ClientSecret == "" {
			errs = append(errs, errors.New("internal_calls.client_secret is required with a gateway_url (YC_IDENTITY_SERVICE_SECRET)"))
		}
		if !strings.Contains(c.Password.ResetURL, "{token}") {
			errs = append(errs, errors.New("password.reset_url must contain {token}"))
		}
	}
	if c.Password.LogResetLinks && c.Environment == "production" {
		errs = append(errs, errors.New("password.log_reset_links is not allowed in production"))
	}
	if c.Invitations.LogInviteLinks && c.Environment == "production" {
		errs = append(errs, errors.New("invitations.log_invite_links is not allowed in production"))
	}
	return errors.Join(errs...)
}
