package app

import (
	"errors"
	"os"

	config "github.com/slaghuis/yc-platform/libs/yc-config"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	defaults "github.com/slaghuis/yc-platform/services/yc-product/config"
)

// Config is the service configuration (config/config.default.yaml).
type Config struct {
	Service     string                `koanf:"service"`
	Environment string                `koanf:"environment"`
	HTTP        httpkit.ServerConfig  `koanf:"http"`
	Log         LogConfig             `koanf:"log"`
	Database    dbkit.Config          `koanf:"database"`
	NATS        NATSConfig            `koanf:"nats"`
	Telemetry   TelemetryConfig       `koanf:"telemetry"`
	Gateway     httpkit.GatewayConfig `koanf:"gateway"`
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

// LoadConfig layers the embedded defaults, the optional file named by
// YC_CONFIG_FILE and YC_PRODUCT__* environment overrides.
func LoadConfig() (Config, error) {
	cfg := Config{HTTP: httpkit.DefaultServerConfig(), Database: dbkit.DefaultConfig()}
	err := config.Load(&cfg,
		config.Bytes("config.default.yaml", defaults.Default),
		config.OptionalFile(os.Getenv("YC_CONFIG_FILE")),
		config.Env("YC_PRODUCT"),
	)
	return cfg, err
}

// Validate reports missing required settings.
func (c *Config) Validate() error {
	var errs []error
	if c.Database.URL == "" {
		errs = append(errs, errors.New("database.url is required (YC_PRODUCT_DB_URL)"))
	}
	if c.NATS.URL == "" {
		errs = append(errs, errors.New("nats.url is required (NATS_URL)"))
	}
	if c.Gateway.InternalURL != "" && c.Gateway.AdvertiseURL == "" {
		errs = append(errs, errors.New("gateway.advertise_url is required with gateway.internal_url"))
	}
	return errors.Join(errs...)
}
