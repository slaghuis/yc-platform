package app

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	authctx "github.com/slaghuis/yc-platform/libs/yc-authctx"
	config "github.com/slaghuis/yc-platform/libs/yc-config"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	defaults "github.com/slaghuis/yc-platform/services/yc-gateway/config"
	"github.com/slaghuis/yc-platform/services/yc-gateway/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-gateway/internal/service"
	transport "github.com/slaghuis/yc-platform/services/yc-gateway/internal/transport/http"
)

// Config is the service configuration (config/config.default.yaml).
type Config struct {
	Service            string                 `koanf:"service"`
	Environment        string                 `koanf:"environment"`
	HTTP               httpkit.ServerConfig   `koanf:"http"`
	InternalHTTP       httpkit.ServerConfig   `koanf:"internal_http"`
	Log                LogConfig              `koanf:"log"`
	Database           dbkit.Config           `koanf:"database"`
	NATS               NATSConfig             `koanf:"nats"`
	Telemetry          TelemetryConfig        `koanf:"telemetry"`
	Upstreams          map[string]string      `koanf:"upstreams"`
	Routes             []RouteConfig          `koanf:"routes"`
	Registry           service.RegistryConfig `koanf:"registry"`
	PlatformHosts      []string               `koanf:"platform_hosts"`
	TenantHosts        []TenantHostConfig     `koanf:"tenant_hosts"`
	TenantHostsRefresh time.Duration          `koanf:"tenant_hosts_refresh"`
	TokenCacheTTL      time.Duration          `koanf:"token_cache_ttl"`
	IdentityTimeout    time.Duration          `koanf:"identity_timeout"`
	VisitorCookie      transport.CookieConfig `koanf:"visitor_cookie"`
}

// LogConfig is the `log:` block.
type LogConfig struct {
	Level string `koanf:"level"`
}

// NATSConfig is the `nats:` block.
type NATSConfig struct {
	URL string `koanf:"url"`
}

// TelemetryConfig is the `telemetry:` block.
type TelemetryConfig struct {
	OTLPEndpoint string `koanf:"otlp_endpoint"`
}

// RouteConfig is one static route.
type RouteConfig struct {
	Prefix   string   `koanf:"prefix"`
	Methods  []string `koanf:"methods"`
	Upstream string   `koanf:"upstream"` // a key of upstreams
	Roles    []string `koanf:"roles"`
	// Anonymous admits token-less requests on platform hosts.
	Anonymous bool `koanf:"anonymous"`
}

// TenantHostConfig is one static hostname mapping.
type TenantHostConfig struct {
	Hostname string `koanf:"hostname"`
	TenantID string `koanf:"tenant_id"`
}

// identityUpstream is the upstreams key for yc-identity.
const identityUpstream = "identity"

// LoadConfig layers the embedded defaults, the optional file named by
// YC_CONFIG_FILE and YC_GATEWAY__* environment overrides.
func LoadConfig() (Config, error) {
	cfg := Config{
		HTTP:         httpkit.DefaultServerConfig(),
		InternalHTTP: httpkit.DefaultServerConfig(),
		Database:     dbkit.DefaultConfig(),
	}
	err := config.Load(&cfg,
		config.Bytes("config.default.yaml", defaults.Default),
		config.OptionalFile(os.Getenv("YC_CONFIG_FILE")),
		config.Env("YC_GATEWAY"),
	)
	return cfg, err
}

// Validate reports missing or inconsistent settings.
func (c *Config) Validate() error {
	var errs []error
	if c.Database.URL == "" {
		errs = append(errs, errors.New("database.url is required (YC_GATEWAY_DB_URL)"))
	}
	if c.NATS.URL == "" {
		errs = append(errs, errors.New("nats.url is required (NATS_URL)"))
	}
	if c.Upstreams[identityUpstream] == "" {
		errs = append(errs, errors.New("upstreams.identity is required"))
	}
	if c.TokenCacheTTL <= 0 || c.IdentityTimeout <= 0 || c.TenantHostsRefresh <= 0 {
		errs = append(errs, errors.New("token_cache_ttl, identity_timeout and tenant_hosts_refresh must be positive"))
	}
	if c.VisitorCookie.Name == "" {
		errs = append(errs, errors.New("visitor_cookie.name is required"))
	}
	if c.InternalHTTP.Listen == "" || c.InternalHTTP.Listen == c.HTTP.Listen {
		errs = append(errs, errors.New("internal_http.listen is required and must differ from http.listen"))
	}
	if c.Registry.HealthInterval <= 0 || c.Registry.HealthTimeout <= 0 || c.Registry.FailureThreshold < 1 {
		errs = append(errs, errors.New("registry.health_interval, health_timeout and failure_threshold must be positive"))
	}
	if routes, err := c.routes(); err != nil {
		errs = append(errs, err)
	} else if _, err := domain.NewRoutes(routes); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (c *Config) routes() ([]domain.Route, error) {
	routes := make([]domain.Route, 0, len(c.Routes))
	for _, rc := range c.Routes {
		base, ok := c.Upstreams[rc.Upstream]
		if !ok {
			return nil, fmt.Errorf("route %s: unknown upstream %q", rc.Prefix, rc.Upstream)
		}
		u, err := url.Parse(base)
		if err != nil {
			return nil, fmt.Errorf("upstream %s: %w", rc.Upstream, err)
		}
		roles := make([]authctx.ActorType, 0, len(rc.Roles))
		for _, r := range rc.Roles {
			roles = append(roles, authctx.ActorType(r))
		}
		routes = append(routes, domain.Route{
			Prefix: rc.Prefix, Methods: rc.Methods, Upstream: u, Roles: roles, Anonymous: rc.Anonymous,
		})
	}
	return routes, nil
}

func (c *Config) staticHosts() map[string]string {
	m := make(map[string]string, len(c.TenantHosts))
	for _, h := range c.TenantHosts {
		m[h.Hostname] = h.TenantID
	}
	return m
}
