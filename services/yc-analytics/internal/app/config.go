package app

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	config "github.com/slaghuis/yc-platform/libs/yc-config"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	defaults "github.com/slaghuis/yc-platform/services/yc-analytics/config"
	"github.com/slaghuis/yc-platform/services/yc-analytics/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-analytics/internal/service"
)

// Config is the service configuration (config/config.default.yaml).
type Config struct {
	Service      string                `koanf:"service"`
	Environment  string                `koanf:"environment"`
	HTTP         httpkit.ServerConfig  `koanf:"http"`
	Log          LogConfig             `koanf:"log"`
	Database     dbkit.Config          `koanf:"database"`
	NATS         NATSConfig            `koanf:"nats"`
	Telemetry    TelemetryConfig       `koanf:"telemetry"`
	Gateway      httpkit.GatewayConfig `koanf:"gateway"`
	Aggregates   AggregatesConfig      `koanf:"aggregates"`
	Funnel       FunnelConfig          `koanf:"funnel"`
	CartRecovery CartRecoveryConfig    `koanf:"cart_recovery"`
	Milestones   MilestonesConfig      `koanf:"milestones"`
}

// CartRecoveryConfig is the `cart_recovery:` block (issue #35).
type CartRecoveryConfig struct {
	Window time.Duration `koanf:"window"`
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

// AggregatesConfig is the `aggregates:` block.
type AggregatesConfig struct {
	RefreshInterval time.Duration `koanf:"refresh_interval"`
	RefreshBatch    int           `koanf:"refresh_batch"`
	// NightlyAt is a UTC time of day, HH:MM.
	NightlyAt   string `koanf:"nightly_at"`
	NightlyDays int    `koanf:"nightly_days"`
}

// FunnelConfig is the `funnel:` block.
type FunnelConfig struct {
	CartTypes []string `koanf:"cart_types"`
}

// MilestonesConfig is the `milestones:` block.
type MilestonesConfig struct {
	FirstOrder   bool          `koanf:"first_order"`
	OrderCounts  []int64       `koanf:"order_counts"`
	RevenueTiers []int64       `koanf:"revenue_tiers"`
	MaxAge       time.Duration `koanf:"max_age"`
}

// LoadConfig layers the embedded defaults, the optional file named by
// YC_CONFIG_FILE and YC_ANALYTICS__* environment overrides.
func LoadConfig() (Config, error) {
	cfg := Config{HTTP: httpkit.DefaultServerConfig(), Database: dbkit.DefaultConfig()}
	err := config.Load(&cfg,
		config.Bytes("config.default.yaml", defaults.Default),
		config.OptionalFile(os.Getenv("YC_CONFIG_FILE")),
		config.Env("YC_ANALYTICS"),
	)
	return cfg, err
}

// Validate reports missing or invalid settings.
func (c *Config) Validate() error {
	var errs []error
	if c.Database.URL == "" {
		errs = append(errs, errors.New("database.url is required (YC_ANALYTICS_DB_URL)"))
	}
	if c.NATS.URL == "" {
		errs = append(errs, errors.New("nats.url is required (NATS_URL)"))
	}
	if c.Gateway.InternalURL != "" && c.Gateway.AdvertiseURL == "" {
		errs = append(errs, errors.New("gateway.advertise_url is required with gateway.internal_url"))
	}
	a := c.Aggregates
	if a.RefreshInterval <= 0 || a.RefreshBatch < 1 || a.NightlyDays < 1 {
		errs = append(errs, errors.New("aggregates.refresh_interval, refresh_batch and nightly_days must be positive"))
	}
	if _, err := c.nightlyAt(); err != nil {
		errs = append(errs, err)
	}
	if len(c.Funnel.CartTypes) == 0 {
		errs = append(errs, errors.New("funnel.cart_types must name at least one cart type"))
	}
	m := c.Milestones
	for _, list := range [][]int64{m.OrderCounts, m.RevenueTiers} {
		if !slices.IsSorted(list) || slices.ContainsFunc(list, func(n int64) bool { return n < 1 }) {
			errs = append(errs, errors.New("milestones.order_counts and revenue_tiers must be positive and ascending"))
			break
		}
	}
	if m.MaxAge < 0 {
		errs = append(errs, errors.New("milestones.max_age must not be negative"))
	}
	if c.CartRecovery.Window <= 0 {
		errs = append(errs, errors.New("cart_recovery.window must be positive"))
	}
	return errors.Join(errs...)
}

// nightlyAt is aggregates.nightly_at as an offset from midnight UTC.
func (c *Config) nightlyAt() (time.Duration, error) {
	t, err := time.Parse("15:04", c.Aggregates.NightlyAt)
	if err != nil {
		return 0, fmt.Errorf("aggregates.nightly_at %q is not HH:MM", c.Aggregates.NightlyAt)
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute, nil
}

func (c *Config) settings() service.Config {
	m := c.Milestones
	return service.Config{
		Milestones: domain.MilestoneRules{
			FirstOrder: m.FirstOrder, OrderCounts: m.OrderCounts, RevenueTiers: m.RevenueTiers, MaxAge: m.MaxAge,
		},
		FunnelCartTypes: c.Funnel.CartTypes,
		RecoveryWindow:  c.CartRecovery.Window,
		RefreshBatch:    c.Aggregates.RefreshBatch,
		NightlyDays:     c.Aggregates.NightlyDays,
	}
}
