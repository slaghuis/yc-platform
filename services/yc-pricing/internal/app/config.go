package app

import (
	"errors"
	"fmt"
	"os"
	"time"

	config "github.com/slaghuis/yc-platform/libs/yc-config"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	money "github.com/slaghuis/yc-platform/libs/yc-money"
	defaults "github.com/slaghuis/yc-platform/services/yc-pricing/config"
	"github.com/slaghuis/yc-platform/services/yc-pricing/internal/integration"
	"github.com/slaghuis/yc-platform/services/yc-pricing/internal/service"
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
	Rates       RatesConfig           `koanf:"rates"`
}

// RatesConfig is the `rates:` block: the exchange rate provider and the
// refresh schedule.
type RatesConfig struct {
	Provider         string                   `koanf:"provider"`
	FrankfurterURL   string                   `koanf:"frankfurter_url"`
	ExchangerateHost integration.ExchangeHost `koanf:"exchangerate_host"`
	Timeout          time.Duration            `koanf:"timeout"`
	RefreshInterval  time.Duration            `koanf:"refresh_interval"`
	StaleAfter       time.Duration            `koanf:"stale_after"`
	BaseCurrencies   []string                 `koanf:"base_currencies"`
}

func (r RatesConfig) provider() integration.RatesConfig {
	return integration.RatesConfig{
		Provider: r.Provider, FrankfurterURL: r.FrankfurterURL, ExchangeHost: r.ExchangerateHost, Timeout: r.Timeout,
	}
}

func (r RatesConfig) refresh() service.RefreshConfig {
	return service.RefreshConfig{Interval: r.RefreshInterval, StaleAfter: r.StaleAfter, BaseCurrencies: r.BaseCurrencies}
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
// YC_CONFIG_FILE and YC_PRICING__* environment overrides.
func LoadConfig() (Config, error) {
	cfg := Config{HTTP: httpkit.DefaultServerConfig(), Database: dbkit.DefaultConfig()}
	err := config.Load(&cfg,
		config.Bytes("config.default.yaml", defaults.Default),
		config.OptionalFile(os.Getenv("YC_CONFIG_FILE")),
		config.Env("YC_PRICING"),
	)
	return cfg, err
}

// Validate reports missing required settings.
func (c *Config) Validate() error {
	var errs []error
	if c.Database.URL == "" {
		errs = append(errs, errors.New("database.url is required (YC_PRICING_DB_URL)"))
	}
	if c.NATS.URL == "" {
		errs = append(errs, errors.New("nats.url is required (NATS_URL)"))
	}
	if c.Gateway.InternalURL != "" && c.Gateway.AdvertiseURL == "" {
		errs = append(errs, errors.New("gateway.advertise_url is required with gateway.internal_url"))
	}
	if c.Rates.Timeout <= 0 || c.Rates.RefreshInterval <= 0 || c.Rates.StaleAfter <= 0 {
		errs = append(errs, errors.New("rates.timeout, rates.refresh_interval and rates.stale_after must be positive"))
	}
	for _, cur := range c.Rates.BaseCurrencies {
		if !money.ValidCurrency(cur) {
			errs = append(errs, fmt.Errorf("rates.base_currencies: %q is not an ISO 4217 code", cur))
		}
	}
	if _, err := integration.NewRateProvider(c.Rates.provider()); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
