package app

import (
	"errors"
	"os"
	"time"

	config "github.com/slaghuis/yc-platform/libs/yc-config"
	dbkit "github.com/slaghuis/yc-platform/libs/yc-dbkit"
	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	defaults "github.com/slaghuis/yc-platform/services/yc-identity/config"
	"github.com/slaghuis/yc-platform/services/yc-identity/internal/password"
)

// Config is the service configuration (config/config.default.yaml).
type Config struct {
	Service     string               `koanf:"service"`
	Environment string               `koanf:"environment"`
	HTTP        httpkit.ServerConfig `koanf:"http"`
	Log         LogConfig            `koanf:"log"`
	Database    dbkit.Config         `koanf:"database"`
	NATS        NATSConfig           `koanf:"nats"`
	Telemetry   TelemetryConfig      `koanf:"telemetry"`
	JWT         JWTConfig            `koanf:"jwt"`
	Password    PasswordConfig       `koanf:"password"`
}

// PasswordConfig is the `password:` block.
type PasswordConfig struct {
	Argon2 password.Params `koanf:"argon2"`
}

// LogConfig is the `log:` block.
type LogConfig struct {
	Level string `koanf:"level"`
}

// NATSConfig is the `nats:` block.
type NATSConfig struct {
	URL string `koanf:"url"`

	// DisableRelay leaves published events in the outbox. Only for the
	// outbox crash drill (scripts/demo-phase1.sh).
	DisableRelay bool `koanf:"disable_relay"`
}

// TelemetryConfig is the `telemetry:` block.
type TelemetryConfig struct {
	OTLPEndpoint string `koanf:"otlp_endpoint"`
}

// JWTConfig is the `jwt:` block.
type JWTConfig struct {
	Issuer          string        `koanf:"issuer"`
	PrivateKeyFile  string        `koanf:"private_key_file"`
	AccessTokenTTL  time.Duration `koanf:"access_token_ttl"`
	VisitorTokenTTL time.Duration `koanf:"visitor_token_ttl"`
}

// LoadConfig layers the embedded defaults, the optional file named by
// YC_CONFIG_FILE and YC_IDENTITY__* environment overrides.
func LoadConfig() (Config, error) {
	cfg := Config{HTTP: httpkit.DefaultServerConfig(), Database: dbkit.DefaultConfig()}
	err := config.Load(&cfg,
		config.Bytes("config.default.yaml", defaults.Default),
		config.OptionalFile(os.Getenv("YC_CONFIG_FILE")),
		config.Env("YC_IDENTITY"),
	)
	return cfg, err
}

// Validate reports missing required settings.
func (c *Config) Validate() error {
	var errs []error
	if c.Database.URL == "" {
		errs = append(errs, errors.New("database.url is required (YC_IDENTITY_DB_URL)"))
	}
	if c.NATS.URL == "" {
		errs = append(errs, errors.New("nats.url is required (NATS_URL)"))
	}
	if c.JWT.PrivateKeyFile == "" {
		errs = append(errs, errors.New("jwt.private_key_file is required (YC_IDENTITY_JWT_PRIVATE_KEY_FILE)"))
	}
	if c.JWT.VisitorTokenTTL <= 0 || c.JWT.AccessTokenTTL <= 0 {
		errs = append(errs, errors.New("jwt token TTLs must be positive"))
	}
	if err := c.Password.Argon2.Validate(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
