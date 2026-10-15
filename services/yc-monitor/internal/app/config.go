package app

import (
	"errors"
	"net/url"
	"os"
	"time"

	config "github.com/slaghuis/yc-platform/libs/yc-config"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	defaults "github.com/slaghuis/yc-platform/services/yc-monitor/config"
	"github.com/slaghuis/yc-platform/services/yc-monitor/internal/domain"
	"github.com/slaghuis/yc-platform/services/yc-monitor/internal/repository"
	"github.com/slaghuis/yc-platform/services/yc-monitor/internal/service"
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
	HealthCheck HealthCheckConfig     `koanf:"health_check"`
	Alerts      AlertsConfig          `koanf:"alerts"`
	Retention   RetentionConfig       `koanf:"retention"`
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

// HealthCheckConfig is the `health_check:` block.
type HealthCheckConfig struct {
	Interval            time.Duration `koanf:"interval"`
	Timeout             time.Duration `koanf:"timeout"`
	FailuresToUnhealthy int           `koanf:"failures_to_unhealthy"`
	SuccessesToRecover  int           `koanf:"successes_to_recover"`
}

// AlertsConfig is the `alerts:` block.
type AlertsConfig struct {
	EvaluateInterval   time.Duration `koanf:"evaluate_interval"`
	CriticalWebhookURL string        `koanf:"critical_webhook_url"`
	DLQRate            RuleConfig    `koanf:"dlq_rate"`
	CheckoutSuccess    RuleConfig    `koanf:"checkout_success_rate"`
}

// RuleConfig is a threshold rule's default settings.
type RuleConfig struct {
	Threshold     float64 `koanf:"threshold"`
	MinVolume     int64   `koanf:"min_volume"`
	WindowMinutes int     `koanf:"window_minutes"`
}

// RetentionConfig is the `retention:` block.
type RetentionConfig struct {
	PurgeInterval  time.Duration `koanf:"purge_interval"`
	Rollup1m       time.Duration `koanf:"rollup_1m"`
	Rollup5m       time.Duration `koanf:"rollup_5m"`
	Rollup1h       time.Duration `koanf:"rollup_1h"`
	Rollup1d       time.Duration `koanf:"rollup_1d"`
	DeadLetters    time.Duration `koanf:"dead_letters"`
	ReceivedEvents time.Duration `koanf:"received_events"`
}

// LoadConfig layers the embedded defaults, the optional file named by
// YC_CONFIG_FILE and YC_MONITOR__* environment overrides.
func LoadConfig() (Config, error) {
	cfg := Config{HTTP: httpkit.DefaultServerConfig(), Database: dbkit.DefaultConfig()}
	err := config.Load(&cfg,
		config.Bytes("config.default.yaml", defaults.Default),
		config.OptionalFile(os.Getenv("YC_CONFIG_FILE")),
		config.Env("YC_MONITOR"),
	)
	return cfg, err
}

// Validate reports missing or invalid settings.
func (c *Config) Validate() error {
	var errs []error
	if c.Database.URL == "" {
		errs = append(errs, errors.New("database.url is required (YC_MONITOR_DB_URL)"))
	}
	if c.NATS.URL == "" {
		errs = append(errs, errors.New("nats.url is required (NATS_URL)"))
	}
	if c.Gateway.InternalURL != "" && c.Gateway.AdvertiseURL == "" {
		errs = append(errs, errors.New("gateway.advertise_url is required with gateway.internal_url"))
	}
	h := c.HealthCheck
	if h.Interval <= 0 || h.Timeout <= 0 || h.FailuresToUnhealthy < 1 || h.SuccessesToRecover < 1 {
		errs = append(errs, errors.New("health_check: interval and timeout must be positive, counts at least 1"))
	}
	if c.Alerts.EvaluateInterval <= 0 {
		errs = append(errs, errors.New("alerts.evaluate_interval must be positive"))
	}
	if u := c.Alerts.CriticalWebhookURL; u != "" {
		if p, err := url.Parse(u); err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
			errs = append(errs, errors.New("alerts.critical_webhook_url must be an http(s) URL (YC_MONITOR_CRITICAL_WEBHOOK)"))
		}
	}
	for name, r := range map[string]RuleConfig{"dlq_rate": c.Alerts.DLQRate, "checkout_success_rate": c.Alerts.CheckoutSuccess} {
		if r.Threshold < 0 || r.MinVolume < 0 || r.WindowMinutes < 1 || r.WindowMinutes > 1440 {
			errs = append(errs, errors.New("alerts."+name+": threshold and min_volume must be >= 0, window 1-1440 minutes"))
		}
	}
	if c.Alerts.CheckoutSuccess.Threshold > 1 {
		errs = append(errs, errors.New("alerts.checkout_success_rate.threshold is a ratio, 0 to 1"))
	}
	r := c.Retention
	for _, d := range []time.Duration{r.PurgeInterval, r.Rollup1m, r.Rollup5m, r.Rollup1h, r.Rollup1d, r.DeadLetters, r.ReceivedEvents} {
		if d <= 0 {
			errs = append(errs, errors.New("retention: every duration must be positive"))
			break
		}
	}
	return errors.Join(errs...)
}

func (c *Config) settings() service.Config {
	rule := func(name string, r RuleConfig) domain.Thresholds {
		return domain.Thresholds{
			Rule: name, Threshold: r.Threshold, MinVolume: r.MinVolume, WindowMinutes: r.WindowMinutes, Enabled: true,
		}
	}
	return service.Config{
		Health: domain.HealthPolicy{
			FailuresToUnhealthy: c.HealthCheck.FailuresToUnhealthy, SuccessesToRecover: c.HealthCheck.SuccessesToRecover,
		},
		HealthTimeout: c.HealthCheck.Timeout,
		Defaults: map[string]domain.Thresholds{
			domain.RuleDLQRate:             rule(domain.RuleDLQRate, c.Alerts.DLQRate),
			domain.RuleCheckoutSuccessRate: rule(domain.RuleCheckoutSuccessRate, c.Alerts.CheckoutSuccess),
		},
		Retention: repository.Retention{
			domain.Bucket1m: c.Retention.Rollup1m, domain.Bucket5m: c.Retention.Rollup5m,
			domain.Bucket1h: c.Retention.Rollup1h, domain.Bucket1d: c.Retention.Rollup1d,
		},
		DLQRetention:       c.Retention.DeadLetters,
		EventRetention:     c.Retention.ReceivedEvents,
		CriticalWebhookURL: c.Alerts.CriticalWebhookURL,
	}
}
