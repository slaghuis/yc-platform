package app

import (
	"errors"
	"fmt"
	"os"
	"time"

	config "github.com/slaghuis/yc-platform/libs/yc-config"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"

	defaults "github.com/slaghuis/yc-platform/services/yc-payment/config"
	"github.com/slaghuis/yc-platform/services/yc-payment/internal/secrets"
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
	Encryption  EncryptionConfig      `koanf:"encryption"`
	Gateways    GatewaysConfig        `koanf:"gateways"`
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

// EncryptionConfig is the `encryption:` block.
type EncryptionConfig struct {
	CipherKey string `koanf:"cipher_key"`
}

// GatewaysConfig is the `gateways:` block.
type GatewaysConfig struct {
	Stripe DriverConfig `koanf:"stripe"`
	Yoco   DriverConfig `koanf:"yoco"`
}

// DriverConfig is one gateway driver's settings.
type DriverConfig struct {
	APIBase string        `koanf:"api_base"`
	Timeout time.Duration `koanf:"timeout"`
}

// LoadConfig layers the embedded defaults, the optional file named by
// YC_CONFIG_FILE and YC_PAYMENT__* environment overrides.
func LoadConfig() (Config, error) {
	cfg := Config{HTTP: httpkit.DefaultServerConfig(), Database: dbkit.DefaultConfig()}
	err := config.Load(&cfg,
		config.Bytes("config.default.yaml", defaults.Default),
		config.OptionalFile(os.Getenv("YC_CONFIG_FILE")),
		config.Env("YC_PAYMENT"),
	)
	return cfg, err
}

// Validate reports missing or invalid settings.
func (c *Config) Validate() error {
	var errs []error
	if c.Database.URL == "" {
		errs = append(errs, errors.New("database.url is required (YC_PAYMENT_DB_URL)"))
	}
	if c.NATS.URL == "" {
		errs = append(errs, errors.New("nats.url is required (NATS_URL)"))
	}
	if c.Gateway.InternalURL != "" && c.Gateway.AdvertiseURL == "" {
		errs = append(errs, errors.New("gateway.advertise_url is required with gateway.internal_url"))
	}
	if c.Encryption.CipherKey == "" {
		errs = append(errs, errors.New("encryption.cipher_key is required (YC_PAYMENT_CIPHER_KEY)"))
	} else if _, err := secrets.NewCipher(c.Encryption.CipherKey); err != nil {
		errs = append(errs, fmt.Errorf("encryption.cipher_key: %w", err))
	}
	if c.Gateways.Stripe.APIBase == "" || c.Gateways.Stripe.Timeout <= 0 {
		errs = append(errs, errors.New("gateways.stripe.api_base and timeout are required"))
	}
	if c.Gateways.Yoco.APIBase == "" || c.Gateways.Yoco.Timeout <= 0 {
		errs = append(errs, errors.New("gateways.yoco.api_base and timeout are required"))
	}
	return errors.Join(errs...)
}
