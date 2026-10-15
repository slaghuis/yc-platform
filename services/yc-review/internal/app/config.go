package app

import (
	"errors"
	"os"
	"time"

	config "github.com/slaghuis/yc-platform/libs/yc-config"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	defaults "github.com/slaghuis/yc-platform/services/yc-review/config"
	"github.com/slaghuis/yc-platform/services/yc-review/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-review/internal/service"
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
	Defaults      DefaultsConfig         `koanf:"defaults"`
	PurchaseCheck PurchaseCheckConfig    `koanf:"purchase_check"`
	Prompts       PromptsConfig          `koanf:"prompts"`
	SweepInterval time.Duration          `koanf:"sweep_interval"`
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

// DefaultsConfig is the `defaults:` block: settings for shops without
// their own.
type DefaultsConfig struct {
	ModerationMode  string `koanf:"moderation_mode"`
	RequirePurchase bool   `koanf:"require_purchase"`
	AllowEdit       bool   `koanf:"allow_edit"`
}

func (d DefaultsConfig) settings() domain.Settings {
	return domain.Settings{
		ModerationMode: domain.ModerationMode(d.ModerationMode), RequirePurchase: d.RequirePurchase, AllowEdit: d.AllowEdit,
	}
}

// PurchaseCheckConfig is the `purchase_check:` block.
type PurchaseCheckConfig struct {
	Window        time.Duration `koanf:"window"`
	RetryInterval time.Duration `koanf:"retry_interval"`
}

// PromptsConfig is the `prompts:` block.
type PromptsConfig struct {
	Delay time.Duration `koanf:"delay"`
}

// LoadConfig layers the embedded defaults, the optional file named by
// YC_CONFIG_FILE and YC_REVIEW__* environment overrides.
func LoadConfig() (Config, error) {
	cfg := Config{HTTP: httpkit.DefaultServerConfig(), Database: dbkit.DefaultConfig()}
	err := config.Load(&cfg,
		config.Bytes("config.default.yaml", defaults.Default),
		config.OptionalFile(os.Getenv("YC_CONFIG_FILE")),
		config.Env("YC_REVIEW"),
	)
	return cfg, err
}

// Validate reports missing or invalid settings.
func (c *Config) Validate() error {
	var errs []error
	if c.Database.URL == "" {
		errs = append(errs, errors.New("database.url is required (YC_REVIEW_DB_URL)"))
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
		errs = append(errs, errors.New("internal_calls.client_secret is required (YC_REVIEW_SERVICE_SECRET)"))
	}
	if c.Idempotency.Retention < 24*time.Hour {
		errs = append(errs, errors.New("idempotency.retention must be at least 24h"))
	}
	if c.Idempotency.PurgeInterval <= 0 {
		errs = append(errs, errors.New("idempotency.purge_interval must be positive"))
	}
	if err := c.Defaults.settings().Validate(); err != nil {
		errs = append(errs, errors.New("defaults.moderation_mode must be manual or auto"))
	}
	if c.PurchaseCheck.Window <= 0 || c.PurchaseCheck.RetryInterval <= 0 {
		errs = append(errs, errors.New("purchase_check.window and retry_interval must be positive"))
	}
	if c.Prompts.Delay < 0 {
		errs = append(errs, errors.New("prompts.delay must not be negative"))
	}
	if c.SweepInterval <= 0 {
		errs = append(errs, errors.New("sweep_interval must be positive"))
	}
	return errors.Join(errs...)
}

func (c *Config) settings() service.Config {
	return service.Config{
		Defaults:    c.Defaults.settings(),
		CheckWindow: c.PurchaseCheck.Window,
		CheckRetry:  c.PurchaseCheck.RetryInterval,
		PromptDelay: c.Prompts.Delay,
	}
}
