package app

import (
	"errors"
	"fmt"
	"os"
	"time"

	config "github.com/slaghuis/yc-platform/libs/yc-config"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	defaults "github.com/slaghuis/yc-platform/services/yc-inventory/config"
	"github.com/slaghuis/yc-platform/services/yc-inventory/internal/domain"
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
	Reservation ReservationConfig     `koanf:"reservation"`
	Idempotency IdempotencyConfig     `koanf:"idempotency"`
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

// ReservationConfig is the `reservation:` block: defaults for shops
// without settings of their own, and the expiry sweeper.
type ReservationConfig struct {
	Enabled         bool          `koanf:"enabled"`
	DefaultTTL      int           `koanf:"default_ttl_minutes"`
	MinTTL          int           `koanf:"min_ttl_minutes"`
	MaxTTL          int           `koanf:"max_ttl_minutes"`
	SweeperInterval time.Duration `koanf:"sweeper_interval"`
}

func (r ReservationConfig) domain() domain.Config {
	return domain.Config{Enabled: r.Enabled, DefaultTTL: r.DefaultTTL, MinTTL: r.MinTTL, MaxTTL: r.MaxTTL}
}

// IdempotencyConfig is the `idempotency:` block.
type IdempotencyConfig struct {
	Retention     time.Duration `koanf:"retention"`
	PurgeInterval time.Duration `koanf:"purge_interval"`
}

// LoadConfig layers the embedded defaults, the optional file named by
// YC_CONFIG_FILE and YC_INVENTORY__* environment overrides.
func LoadConfig() (Config, error) {
	cfg := Config{HTTP: httpkit.DefaultServerConfig(), Database: dbkit.DefaultConfig()}
	err := config.Load(&cfg,
		config.Bytes("config.default.yaml", defaults.Default),
		config.OptionalFile(os.Getenv("YC_CONFIG_FILE")),
		config.Env("YC_INVENTORY"),
	)
	return cfg, err
}

// Validate reports missing or invalid settings.
func (c *Config) Validate() error {
	var errs []error
	if c.Database.URL == "" {
		errs = append(errs, errors.New("database.url is required (YC_INVENTORY_DB_URL)"))
	}
	if c.NATS.URL == "" {
		errs = append(errs, errors.New("nats.url is required (NATS_URL)"))
	}
	if c.Gateway.InternalURL != "" && c.Gateway.AdvertiseURL == "" {
		errs = append(errs, errors.New("gateway.advertise_url is required with gateway.internal_url"))
	}
	if err := c.Reservation.domain().Validate(); err != nil {
		errs = append(errs, fmt.Errorf("reservation: %w", err))
	}
	if c.Reservation.SweeperInterval <= 0 {
		errs = append(errs, errors.New("reservation.sweeper_interval must be positive"))
	}
	if c.Idempotency.Retention < 24*time.Hour {
		errs = append(errs, errors.New("idempotency.retention must be at least 24h"))
	}
	if c.Idempotency.PurgeInterval <= 0 {
		errs = append(errs, errors.New("idempotency.purge_interval must be positive"))
	}
	return errors.Join(errs...)
}
