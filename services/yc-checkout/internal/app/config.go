package app

import (
	"errors"
	"os"
	"slices"
	"time"

	config "github.com/slaghuis/yc-platform/libs/yc-config"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"

	defaults "github.com/slaghuis/yc-platform/services/yc-checkout/config"
	"github.com/slaghuis/yc-platform/services/yc-checkout/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-checkout/internal/integration"
	"github.com/slaghuis/yc-platform/services/yc-checkout/internal/service"
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
	Calls         CallsConfig            `koanf:"calls"`
	Checkout      CheckoutConfig         `koanf:"checkout"`
	Saga          SagaConfig             `koanf:"saga"`
	Debug         DebugConfig            `koanf:"debug"`
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

// CallsConfig is the `calls:` block.
type CallsConfig struct {
	PaymentTimeout time.Duration `koanf:"payment_timeout"`
	MaxAttempts    int           `koanf:"max_attempts"`
}

// CheckoutConfig is the `checkout:` block.
type CheckoutConfig struct {
	SessionTTL     time.Duration  `koanf:"session_ttl"`
	ReservationTTL time.Duration  `koanf:"reservation_ttl"`
	Gateways       []string       `koanf:"gateways"`
	Shipping       ShippingConfig `koanf:"shipping"`
}

// ShippingConfig is the flat-fee shipping option.
type ShippingConfig struct {
	ID       string `koanf:"id"`
	Name     string `koanf:"name"`
	FeeMinor int64  `koanf:"fee_minor"`
}

// SagaConfig is the `saga:` block.
type SagaConfig struct {
	ConfirmDeadline time.Duration `koanf:"confirm_deadline"`
	LeaseTTL        time.Duration `koanf:"lease_ttl"`
	SweeperInterval time.Duration `koanf:"sweeper_interval"`
	RetryBase       time.Duration `koanf:"retry_base"`
	RetryMax        time.Duration `koanf:"retry_max"`
	PendingPoll     time.Duration `koanf:"pending_poll"`
	AlertAfter      int           `koanf:"alert_after"`
}

// DebugConfig is the `debug:` block (development only).
type DebugConfig struct {
	PauseBefore string        `koanf:"pause_before"`
	PauseFor    time.Duration `koanf:"pause_for"`
}

// LoadConfig layers the embedded defaults, the optional file named by
// YC_CONFIG_FILE and YC_CHECKOUT__* environment overrides.
func LoadConfig() (Config, error) {
	cfg := Config{HTTP: httpkit.DefaultServerConfig(), Database: dbkit.DefaultConfig()}
	err := config.Load(&cfg,
		config.Bytes("config.default.yaml", defaults.Default),
		config.OptionalFile(os.Getenv("YC_CONFIG_FILE")),
		config.Env("YC_CHECKOUT"),
	)
	return cfg, err
}

// Validate reports missing or invalid settings.
func (c *Config) Validate() error {
	var errs []error
	if c.Database.URL == "" {
		errs = append(errs, errors.New("database.url is required (YC_CHECKOUT_DB_URL)"))
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
		errs = append(errs, errors.New("internal_calls.client_secret is required (YC_CHECKOUT_SERVICE_SECRET)"))
	}
	if c.Checkout.SessionTTL < 10*time.Minute || c.Checkout.SessionTTL > time.Hour {
		errs = append(errs, errors.New("checkout.session_ttl must be 10m to 60m (ADR-011)"))
	}
	if c.Checkout.ReservationTTL < time.Minute || c.Checkout.ReservationTTL > c.Checkout.SessionTTL {
		errs = append(errs, errors.New("checkout.reservation_ttl must be at least 1m and at most the session TTL"))
	}
	if len(c.Checkout.Gateways) == 0 || slices.Contains(c.Checkout.Gateways, "") {
		errs = append(errs, errors.New("checkout.gateways must name at least one gateway"))
	}
	if c.Checkout.Shipping.ID == "" || c.Checkout.Shipping.Name == "" || c.Checkout.Shipping.FeeMinor < 0 {
		errs = append(errs, errors.New("checkout.shipping needs an id, a name and a non-negative fee_minor"))
	}
	s := c.Saga
	if s.ConfirmDeadline <= 0 || s.SweeperInterval <= 0 || s.RetryBase <= 0 || s.RetryMax < s.RetryBase || s.PendingPoll <= 0 {
		errs = append(errs, errors.New("saga durations must be positive, with retry_max at least retry_base"))
	}
	if s.LeaseTTL <= s.ConfirmDeadline {
		errs = append(errs, errors.New("saga.lease_ttl must exceed saga.confirm_deadline"))
	}
	if c.Calls.MaxAttempts < 1 || c.Calls.PaymentTimeout <= 0 {
		errs = append(errs, errors.New("calls.max_attempts and calls.payment_timeout must be positive"))
	}
	errs = append(errs, c.validateDebug())
	return errors.Join(errs...)
}

func (c *Config) validateDebug() error {
	if c.Debug.PauseBefore == "" && c.Debug.PauseFor == 0 {
		return nil
	}
	if c.Environment == "production" {
		return errors.New("debug.pause_before is for development only and is refused in production")
	}
	st := domain.State(c.Debug.PauseBefore)
	if !st.Driven() || st == domain.StateCompensating {
		return errors.New("debug.pause_before must be a saga step, e.g. CAPTURING")
	}
	if c.Debug.PauseFor <= 0 || c.Debug.PauseFor >= c.Saga.LeaseTTL {
		return errors.New("debug.pause_for must be positive and shorter than saga.lease_ttl")
	}
	return nil
}

func (c *Config) settings(host string) service.Settings {
	return service.Settings{
		SessionTTL: c.Checkout.SessionTTL, ReservationTTL: c.Checkout.ReservationTTL,
		Shipping: domain.ShippingOption{
			ID: c.Checkout.Shipping.ID, Name: c.Checkout.Shipping.Name, AmountMinor: c.Checkout.Shipping.FeeMinor,
		},
		Gateways: c.Checkout.Gateways, ConfirmDeadline: c.Saga.ConfirmDeadline, LeaseTTL: c.Saga.LeaseTTL,
		RetryBase: c.Saga.RetryBase, RetryMax: c.Saga.RetryMax, PendingPoll: c.Saga.PendingPoll,
		AlertAfter: c.Saga.AlertAfter, PauseBefore: domain.State(c.Debug.PauseBefore), PauseFor: c.Debug.PauseFor,
		Host: host,
	}
}

func (c *Config) timeouts() integration.Timeouts {
	return integration.Timeouts{
		Default: c.InternalCalls.Timeout, Payment: c.Calls.PaymentTimeout, Attempts: c.Calls.MaxAttempts,
	}
}
