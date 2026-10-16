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
	defaults "github.com/slaghuis/yc-platform/services/yc-tenant/config"
	"github.com/slaghuis/yc-platform/services/yc-tenant/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-tenant/internal/service"
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
	// PlatformHosts mirrors yc-gateway's platform_hosts; entries may hold
	// comma-separated lists.
	PlatformHosts []string          `koanf:"platform_hosts"`
	Idempotency   IdempotencyConfig `koanf:"idempotency"`
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
// YC_CONFIG_FILE and YC_TENANT__* environment overrides.
func LoadConfig() (Config, error) {
	cfg := Config{HTTP: httpkit.DefaultServerConfig(), Database: dbkit.DefaultConfig()}
	err := config.Load(&cfg,
		config.Bytes("config.default.yaml", defaults.Default),
		config.OptionalFile(os.Getenv("YC_CONFIG_FILE")),
		config.Env("YC_TENANT"),
	)
	return cfg, err
}

// platformHosts returns the platform hostnames, split, trimmed and
// lowercased.
func (c *Config) platformHosts() []string { return splitList(c.PlatformHosts) }

// splitList expands comma-separated entries (an environment override is one
// entry) into trimmed, lowercased hostnames.
func splitList(items []string) []string {
	var out []string
	for _, item := range items {
		for s := range strings.SplitSeq(item, ",") {
			if s = domain.NormalizeHostname(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// Validate reports missing or invalid settings.
func (c *Config) Validate() error {
	var errs []error
	if c.Database.URL == "" {
		errs = append(errs, errors.New("database.url is required (YC_TENANT_DB_URL)"))
	}
	if c.NATS.URL == "" {
		errs = append(errs, errors.New("nats.url is required (NATS_URL)"))
	}
	if c.Gateway.InternalURL != "" && c.Gateway.AdvertiseURL == "" {
		errs = append(errs, errors.New("gateway.advertise_url is required with gateway.internal_url"))
	}
	for _, h := range c.platformHosts() {
		if !domain.ValidHostname(h) {
			errs = append(errs, fmt.Errorf("platform_hosts: %q is not a hostname", h))
		}
	}
	if c.Idempotency.Retention < 24*time.Hour {
		errs = append(errs, errors.New("idempotency.retention must be at least 24h"))
	}
	if c.Idempotency.PurgeInterval <= 0 {
		errs = append(errs, errors.New("idempotency.purge_interval must be positive"))
	}
	return errors.Join(errs...)
}

func (c *Config) settings() service.Config {
	return service.Config{PlatformHosts: c.platformHosts()}
}
