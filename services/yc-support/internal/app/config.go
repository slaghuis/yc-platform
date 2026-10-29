package app

import (
	"errors"
	"os"
	"time"

	config "github.com/slaghuis/yc-platform/libs/yc-config"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	defaults "github.com/slaghuis/yc-platform/services/yc-support/config"
	"github.com/slaghuis/yc-platform/services/yc-support/internal/service"
)

// Config is the service configuration (config/config.default.yaml).
type Config struct {
	Service       string                 `koanf:"service"`
	Environment   string                 `koanf:"environment"`
	HTTP          httpkit.ServerConfig   `koanf:"http"`
	Log           LogConfig              `koanf:"log"`
	Database      dbkit.Config           `koanf:"database"`
	NATS          NATSConfig             `koanf:"nats"`
	Telemetry     TelemetryConfig        `koanf:"telemetry"`
	Gateway       httpkit.GatewayConfig  `koanf:"gateway"`
	InternalCalls httpkit.InternalConfig `koanf:"internal_calls"`
	Support       SupportConfig          `koanf:"support"`
	Tenant        TenantConfig           `koanf:"tenant"`
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

// SupportConfig is the `support:` block.
type SupportConfig struct {
	IdleTTL        time.Duration `koanf:"idle_ttl"`
	SweepInterval  time.Duration `koanf:"sweep_interval"`
	SweepBatch     int           `koanf:"sweep_batch"`
	StorefrontBase string        `koanf:"storefront_base"`
}

// TenantConfig is the `tenant:` block.
type TenantConfig struct {
	ContactCacheTTL time.Duration `koanf:"contact_cache_ttl"`
}

// LoadConfig layers the embedded defaults, the optional file named by
// YC_CONFIG_FILE and YC_SUPPORT__* environment overrides.
func LoadConfig() (Config, error) {
	cfg := Config{HTTP: httpkit.DefaultServerConfig(), Database: dbkit.DefaultConfig()}
	err := config.Load(&cfg,
		config.Bytes("config.default.yaml", defaults.Default),
		config.OptionalFile(os.Getenv("YC_CONFIG_FILE")),
		config.Env("YC_SUPPORT"),
	)
	return cfg, err
}

// Validate reports missing or invalid settings.
func (c *Config) Validate() error {
	var errs []error
	if c.Database.URL == "" {
		errs = append(errs, errors.New("database.url is required (YC_SUPPORT_DB_URL)"))
	}
	if c.NATS.URL == "" {
		errs = append(errs, errors.New("nats.url is required (NATS_URL)"))
	}
	if c.Gateway.InternalURL != "" && c.Gateway.AdvertiseURL == "" {
		errs = append(errs, errors.New("gateway.advertise_url is required with gateway.internal_url"))
	}
	if c.InternalCalls.GatewayURL == "" {
		errs = append(errs, errors.New("internal_calls.gateway_url is required (YC_GATEWAY_INTERNAL_URL)"))
	}
	if c.InternalCalls.ClientSecret == "" {
		errs = append(errs, errors.New("internal_calls.client_secret is required (YC_SUPPORT_SERVICE_SECRET)"))
	}
	if c.Support.IdleTTL <= 0 {
		errs = append(errs, errors.New("support.idle_ttl must be positive"))
	}
	if c.Support.SweepInterval <= 0 {
		errs = append(errs, errors.New("support.sweep_interval must be positive"))
	}
	if c.Support.SweepBatch <= 0 {
		errs = append(errs, errors.New("support.sweep_batch must be positive"))
	}
	if c.Tenant.ContactCacheTTL < 0 {
		errs = append(errs, errors.New("tenant.contact_cache_ttl must not be negative"))
	}
	return errors.Join(errs...)
}

func (c *Config) settings() service.Config {
	return service.Config{
		IdleTTL: c.Support.IdleTTL, BatchSize: c.Support.SweepBatch, StorefrontBase: c.Support.StorefrontBase,
	}
}
