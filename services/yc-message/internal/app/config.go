package app

import (
	"errors"
	"os"
	"time"

	config "github.com/slaghuis/yc-platform/libs/yc-config"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	defaults "github.com/slaghuis/yc-platform/services/yc-message/config"
	"github.com/slaghuis/yc-platform/services/yc-message/internal/driver"
	"github.com/slaghuis/yc-platform/services/yc-message/internal/service"
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
	SMTP          driver.SMTPConfig      `koanf:"smtp"`
	SMS           SMSConfig              `koanf:"sms"`
	Messages      MessagesConfig         `koanf:"messages"`
	Verification  VerificationConfig     `koanf:"verification"`
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

// SMSConfig is the `sms:` block.
type SMSConfig struct {
	Driver      string `koanf:"driver"`
	FakeWebhook bool   `koanf:"fake_webhook"`
}

// SMS drivers.
const (
	SMSFake = "fake"
	SMSNone = "none"
)

// MessagesConfig is the `messages:` block.
type MessagesConfig struct {
	Locale           string        `koanf:"locale"`
	ShopURL          string        `koanf:"shop_url"`
	MaxAttempts      int           `koanf:"max_attempts"`
	RetryBackoff     time.Duration `koanf:"retry_backoff"`
	BatchSize        int           `koanf:"batch_size"`
	DispatchInterval time.Duration `koanf:"dispatch_interval"`
}

// VerificationConfig is the `verification:` block.
type VerificationConfig struct {
	TTL time.Duration `koanf:"ttl"`
}

// LoadConfig layers the embedded defaults, the optional file named by
// YC_CONFIG_FILE and YC_MESSAGE__* environment overrides.
func LoadConfig() (Config, error) {
	cfg := Config{HTTP: httpkit.DefaultServerConfig(), Database: dbkit.DefaultConfig()}
	err := config.Load(&cfg,
		config.Bytes("config.default.yaml", defaults.Default),
		config.OptionalFile(os.Getenv("YC_CONFIG_FILE")),
		config.Env("YC_MESSAGE"),
	)
	return cfg, err
}

// Validate reports missing or invalid settings.
func (c *Config) Validate() error {
	var errs []error
	if c.Database.URL == "" {
		errs = append(errs, errors.New("database.url is required (YC_MESSAGE_DB_URL)"))
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
		errs = append(errs, errors.New("internal_calls.client_secret is required (YC_MESSAGE_SERVICE_SECRET)"))
	}
	if c.Idempotency.Retention < 24*time.Hour {
		errs = append(errs, errors.New("idempotency.retention must be at least 24h"))
	}
	if c.Idempotency.PurgeInterval <= 0 {
		errs = append(errs, errors.New("idempotency.purge_interval must be positive"))
	}
	if c.SMTP.Addr == "" || c.SMTP.From == "" {
		errs = append(errs, errors.New("smtp.addr and smtp.from are required (SMTP_ADDR)"))
	}
	switch c.SMS.Driver {
	case SMSFake, SMSNone:
	default:
		errs = append(errs, errors.New("sms.driver must be fake or none"))
	}
	if c.Environment == "production" && (c.SMS.Driver == SMSFake || c.SMS.FakeWebhook) {
		errs = append(errs, errors.New("the fake SMS driver and webhook are not allowed in production"))
	}
	if c.Messages.ShopURL == "" {
		errs = append(errs, errors.New("messages.shop_url is required"))
	}
	if c.Messages.MaxAttempts < 1 || c.Messages.BatchSize < 1 {
		errs = append(errs, errors.New("messages.max_attempts and messages.batch_size must be at least 1"))
	}
	if c.Messages.RetryBackoff <= 0 || c.Messages.DispatchInterval <= 0 {
		errs = append(errs, errors.New("messages.retry_backoff and messages.dispatch_interval must be positive"))
	}
	if c.Verification.TTL < time.Minute {
		errs = append(errs, errors.New("verification.ttl must be at least 1m"))
	}
	return errors.Join(errs...)
}

func (c *Config) settings() service.Config {
	locale := c.Messages.Locale
	if locale == "" {
		locale = "en"
	}
	return service.Config{
		Locale: locale, ShopURL: c.Messages.ShopURL, MaxAttempts: c.Messages.MaxAttempts,
		RetryBackoff: c.Messages.RetryBackoff, BatchSize: c.Messages.BatchSize,
		VerificationTTL: c.Verification.TTL, FakeWebhook: c.SMS.FakeWebhook,
	}
}
