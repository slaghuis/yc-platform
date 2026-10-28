package app

import (
	"errors"
	"os"
	"time"

	config "github.com/slaghuis/yc-platform/libs/yc-config"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"

	defaults "github.com/slaghuis/yc-platform/services/yc-orders/config"
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
	Idempotency   IdempotencyConfig      `koanf:"idempotency"`
	GuestAccess   GuestAccessConfig      `koanf:"guest_access"`
}

// GuestAccessConfig is the `guest_access:` block (issue #22).
type GuestAccessConfig struct {
	Secret string        `koanf:"secret"`
	TTL    time.Duration `koanf:"ttl"`
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

// IdempotencyConfig is the `idempotency:` block.
type IdempotencyConfig struct {
	Retention     time.Duration `koanf:"retention"`
	PurgeInterval time.Duration `koanf:"purge_interval"`
}

// LoadConfig layers the embedded defaults, the optional file named by
// YC_CONFIG_FILE and YC_ORDERS__* environment overrides.
func LoadConfig() (Config, error) {
	cfg := Config{HTTP: httpkit.DefaultServerConfig(), Database: dbkit.DefaultConfig()}
	err := config.Load(&cfg,
		config.Bytes("config.default.yaml", defaults.Default),
		config.OptionalFile(os.Getenv("YC_CONFIG_FILE")),
		config.Env("YC_ORDERS"),
	)
	return cfg, err
}

// Validate reports missing or invalid settings.
func (c *Config) Validate() error {
	var errs []error
	if c.Database.URL == "" {
		errs = append(errs, errors.New("database.url is required (YC_ORDERS_DB_URL)"))
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
		errs = append(errs, errors.New("internal_calls.client_secret is required (YC_ORDERS_SERVICE_SECRET)"))
	}
	if c.Idempotency.Retention < 24*time.Hour {
		errs = append(errs, errors.New("idempotency.retention must be at least 24h"))
	}
	if c.Idempotency.PurgeInterval <= 0 {
		errs = append(errs, errors.New("idempotency.purge_interval must be positive"))
	}
	if len(c.GuestAccess.Secret) < 32 {
		errs = append(errs, errors.New("guest_access.secret must be at least 32 characters (YC_ORDERS__GUEST_ACCESS__SECRET)"))
	}
	if c.GuestAccess.TTL < time.Hour {
		errs = append(errs, errors.New("guest_access.ttl must be at least 1h"))
	}
	return errors.Join(errs...)
}
