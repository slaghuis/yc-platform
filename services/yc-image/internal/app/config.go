package app

import (
	"errors"
	"os"

	config "github.com/slaghuis/yc-platform/libs/yc-config"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	defaults "github.com/slaghuis/yc-platform/services/yc-image/config"
	"github.com/slaghuis/yc-platform/services/yc-image/internal/imaging"
	"github.com/slaghuis/yc-platform/services/yc-image/internal/integration"
)

// Config is the service configuration (config/config.default.yaml).
type Config struct {
	Service     string                    `koanf:"service"`
	Environment string                    `koanf:"environment"`
	HTTP        httpkit.ServerConfig      `koanf:"http"`
	Log         LogConfig                 `koanf:"log"`
	Database    dbkit.Config              `koanf:"database"`
	NATS        NATSConfig                `koanf:"nats"`
	Telemetry   TelemetryConfig           `koanf:"telemetry"`
	Gateway     httpkit.GatewayConfig     `koanf:"gateway"`
	Storage     integration.StorageConfig `koanf:"storage"`
	Limits      imaging.Limits            `koanf:"limits"`
	Processing  imaging.Quality           `koanf:"processing"`
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
// YC_CONFIG_FILE and YC_IMAGE__* environment overrides.
func LoadConfig() (Config, error) {
	cfg := Config{HTTP: httpkit.DefaultServerConfig(), Database: dbkit.DefaultConfig()}
	err := config.Load(&cfg,
		config.Bytes("config.default.yaml", defaults.Default),
		config.OptionalFile(os.Getenv("YC_CONFIG_FILE")),
		config.Env("YC_IMAGE"),
	)
	return cfg, err
}

// Validate reports missing required settings.
func (c *Config) Validate() error {
	var errs []error
	if c.Database.URL == "" {
		errs = append(errs, errors.New("database.url is required (YC_IMAGE_DB_URL)"))
	}
	if c.NATS.URL == "" {
		errs = append(errs, errors.New("nats.url is required (NATS_URL)"))
	}
	if c.Gateway.InternalURL != "" && c.Gateway.AdvertiseURL == "" {
		errs = append(errs, errors.New("gateway.advertise_url is required with gateway.internal_url"))
	}
	if c.Storage.Endpoint == "" || c.Storage.Bucket == "" {
		errs = append(errs, errors.New("storage.endpoint and storage.bucket are required (S3_ENDPOINT, S3_BUCKET_IMAGES)"))
	}
	if c.Storage.AccessKey == "" || c.Storage.SecretKey == "" {
		errs = append(errs, errors.New("storage credentials are required (S3_ACCESS_KEY_ID, S3_SECRET_ACCESS_KEY)"))
	}
	if c.Storage.PresignTTL <= 0 {
		errs = append(errs, errors.New("storage.presign_ttl must be positive"))
	}
	if c.Limits.MaxBytes <= 0 || c.Limits.MaxDimension <= 0 || c.Limits.MaxPixels <= 0 || c.Limits.MaxAspectRatio < 1 {
		errs = append(errs, errors.New("limits must be positive (max_aspect_ratio at least 1)"))
	}
	if !quality(c.Processing.JPEG) || !quality(c.Processing.WebP) {
		errs = append(errs, errors.New("processing qualities must be 1-100"))
	}
	return errors.Join(errs...)
}

func quality(q int) bool { return q >= 1 && q <= 100 }
