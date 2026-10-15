package app

import (
	"errors"
	"os"
	"regexp"
	"time"

	config "github.com/slaghuis/yc-platform/libs/yc-config"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	defaults "github.com/slaghuis/yc-platform/services/yc-audit/config"
	"github.com/slaghuis/yc-platform/services/yc-audit/internal/service"
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
	GDPR        GDPRConfig            `koanf:"gdpr"`
	Export      ExportConfig          `koanf:"export"`
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

// GDPRConfig is the `gdpr:` block.
type GDPRConfig struct {
	ExpectedServices []string      `koanf:"expected_services"`
	Deadline         time.Duration `koanf:"deadline"`
	SweepInterval    time.Duration `koanf:"sweep_interval"`
}

// ExportConfig is the `export:` block.
type ExportConfig struct {
	MaxRecords int `koanf:"max_records"`
}

// LoadConfig layers the embedded defaults, the optional file named by
// YC_CONFIG_FILE and YC_AUDIT__* environment overrides.
func LoadConfig() (Config, error) {
	cfg := Config{HTTP: httpkit.DefaultServerConfig(), Database: dbkit.DefaultConfig()}
	err := config.Load(&cfg,
		config.Bytes("config.default.yaml", defaults.Default),
		config.OptionalFile(os.Getenv("YC_CONFIG_FILE")),
		config.Env("YC_AUDIT"),
	)
	return cfg, err
}

var serviceNameRE = regexp.MustCompile(`^yc-[a-z]+(-[a-z]+)*$`)

// Validate reports missing or invalid settings.
func (c *Config) Validate() error {
	var errs []error
	if c.Database.URL == "" {
		errs = append(errs, errors.New("database.url is required (YC_AUDIT_DB_URL)"))
	}
	if c.NATS.URL == "" {
		errs = append(errs, errors.New("nats.url is required (NATS_URL)"))
	}
	if c.Gateway.InternalURL != "" && c.Gateway.AdvertiseURL == "" {
		errs = append(errs, errors.New("gateway.advertise_url is required with gateway.internal_url"))
	}
	if len(c.GDPR.ExpectedServices) == 0 {
		errs = append(errs, errors.New("gdpr.expected_services must name at least one service"))
	}
	for _, s := range c.GDPR.ExpectedServices {
		if !serviceNameRE.MatchString(s) {
			errs = append(errs, errors.New("gdpr.expected_services: "+s+" is not a service name"))
		}
	}
	if c.GDPR.Deadline <= 0 || c.GDPR.SweepInterval <= 0 {
		errs = append(errs, errors.New("gdpr.deadline and gdpr.sweep_interval must be positive"))
	}
	if c.Export.MaxRecords < 1 {
		errs = append(errs, errors.New("export.max_records must be at least 1"))
	}
	return errors.Join(errs...)
}

func (c *Config) settings() service.Config {
	return service.Config{
		ExpectedServices: c.GDPR.ExpectedServices, Deadline: c.GDPR.Deadline, MaxExport: c.Export.MaxRecords,
	}
}
