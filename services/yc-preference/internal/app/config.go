package app

import (
	"errors"
	"fmt"
	"os"

	config "github.com/slaghuis/yc-platform/libs/yc-config"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	defaults "github.com/slaghuis/yc-platform/services/yc-preference/config"
	"github.com/slaghuis/yc-platform/services/yc-preference/internal/domain"
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
	Defaults      DefaultsConfig         `koanf:"defaults"`
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

// DefaultsConfig is the `defaults:` block: preferences for shops without
// defaults of their own.
type DefaultsConfig struct {
	DisplayCurrency string `koanf:"display_currency"`
	Locale          string `koanf:"locale"`
}

func (d DefaultsConfig) domain() domain.Defaults {
	return domain.Defaults{DisplayCurrency: d.DisplayCurrency, Locale: d.Locale}
}

// LoadConfig layers the embedded defaults, the optional file named by
// YC_CONFIG_FILE and YC_PREFERENCE__* environment overrides.
func LoadConfig() (Config, error) {
	cfg := Config{HTTP: httpkit.DefaultServerConfig(), Database: dbkit.DefaultConfig()}
	err := config.Load(&cfg,
		config.Bytes("config.default.yaml", defaults.Default),
		config.OptionalFile(os.Getenv("YC_CONFIG_FILE")),
		config.Env("YC_PREFERENCE"),
	)
	return cfg, err
}

// Validate reports missing or invalid settings.
func (c *Config) Validate() error {
	var errs []error
	if c.Database.URL == "" {
		errs = append(errs, errors.New("database.url is required (YC_PREFERENCE_DB_URL)"))
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
		errs = append(errs, errors.New("internal_calls.client_secret is required (YC_PREFERENCE_SERVICE_SECRET)"))
	}
	if err := c.Defaults.domain().Validate(); err != nil {
		errs = append(errs, fmt.Errorf("defaults: %w", err))
	}
	return errors.Join(errs...)
}
