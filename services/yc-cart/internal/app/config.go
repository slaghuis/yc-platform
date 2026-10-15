package app

import (
	"errors"
	"fmt"
	"os"

	config "github.com/slaghuis/yc-platform/libs/yc-config"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	defaults "github.com/slaghuis/yc-platform/services/yc-cart/config"
	"github.com/slaghuis/yc-platform/services/yc-cart/internal/domain"
)

// Config is the service configuration (config/config.default.yaml).
type Config struct {
	Service       string                    `koanf:"service"`
	Environment   string                    `koanf:"environment"`
	HTTP          httpkit.ServerConfig      `koanf:"http"`
	Log           LogConfig                 `koanf:"log"`
	Database      dbkit.Config              `koanf:"database"`
	NATS          NATSConfig                `koanf:"nats"`
	Telemetry     TelemetryConfig           `koanf:"telemetry"`
	Gateway       httpkit.GatewayConfig     `koanf:"gateway"`
	InternalCalls httpkit.InternalConfig    `koanf:"internal_calls"`
	CartTypes     map[string]CartTypeConfig `koanf:"cart_types"`
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

// CartTypeConfig is one entry of the `cart_types:` block.
type CartTypeConfig struct {
	Convertible  bool `koanf:"convertible"`
	MergeOnLogin bool `koanf:"merge_on_login"`
	MaxItems     int  `koanf:"max_items"`
	MaxQuantity  int  `koanf:"max_quantity"`
}

func (c *Config) types() domain.Types {
	out := make(domain.Types, len(c.CartTypes))
	for name, t := range c.CartTypes {
		out[name] = domain.CartType{
			Name: name, Convertible: t.Convertible, MergeOnLogin: t.MergeOnLogin,
			MaxItems: t.MaxItems, MaxQuantity: t.MaxQuantity,
		}
	}
	return out
}

// LoadConfig layers the embedded defaults, the optional file named by
// YC_CONFIG_FILE and YC_CART__* environment overrides.
func LoadConfig() (Config, error) {
	cfg := Config{HTTP: httpkit.DefaultServerConfig(), Database: dbkit.DefaultConfig()}
	err := config.Load(&cfg,
		config.Bytes("config.default.yaml", defaults.Default),
		config.OptionalFile(os.Getenv("YC_CONFIG_FILE")),
		config.Env("YC_CART"),
	)
	return cfg, err
}

// Validate reports missing or invalid settings.
func (c *Config) Validate() error {
	var errs []error
	if c.Database.URL == "" {
		errs = append(errs, errors.New("database.url is required (YC_CART_DB_URL)"))
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
		errs = append(errs, errors.New("internal_calls.client_secret is required (YC_CART_SERVICE_SECRET)"))
	}
	if err := c.types().Validate(); err != nil {
		errs = append(errs, fmt.Errorf("cart_types: %w", err))
	}
	return errors.Join(errs...)
}
