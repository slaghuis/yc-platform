package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	config "github.com/slaghuis/yc-platform/libs/yc-config"
)

type testConfig struct {
	Service string `koanf:"service"`
	HTTP    struct {
		Listen       string        `koanf:"listen"`
		ReadTimeout  time.Duration `koanf:"read_timeout"`
		MaxBodyBytes int           `koanf:"max_body_bytes"`
	} `koanf:"http"`
	Database struct {
		URL string `koanf:"url"`
	} `koanf:"database"`
	Hosts []struct {
		Hostname string `koanf:"hostname"`
		TenantID string `koanf:"tenant_id"`
	} `koanf:"hosts"`
	Strict bool `koanf:"strict"`
}

func (c *testConfig) Validate() error {
	if c.Strict && c.Database.URL == "" {
		return errors.New("database.url is required")
	}
	return nil
}

const defaultYAML = `
service: yc-test
http:
  listen: ":8080"
  read_timeout: 5s
database:
  url: ${YC_TEST_DB_URL}
hosts:
  - hostname: demo.localhost
    tenant_id: demo
`

func TestLoadExpandsEnvAndKeepsDefaults(t *testing.T) {
	t.Setenv("YC_TEST_DB_URL", "postgres://x")

	cfg := testConfig{}
	cfg.HTTP.MaxBodyBytes = 1024
	require.NoError(t, config.Load(&cfg, config.Bytes("default", []byte(defaultYAML))))

	assert.Equal(t, "yc-test", cfg.Service)
	assert.Equal(t, ":8080", cfg.HTTP.Listen)
	assert.Equal(t, 5*time.Second, cfg.HTTP.ReadTimeout)
	assert.Equal(t, 1024, cfg.HTTP.MaxBodyBytes, "unset keys keep the preset default")
	assert.Equal(t, "postgres://x", cfg.Database.URL)
	require.Len(t, cfg.Hosts, 1)
	assert.Equal(t, "demo", cfg.Hosts[0].TenantID)
}

func TestEnvOverridesNestedKeys(t *testing.T) {
	t.Setenv("YC_TEST__HTTP__LISTEN", ":9090")
	t.Setenv("YC_TEST__HTTP__MAX_BODY_BYTES", "2048")
	t.Setenv("YC_TEST__HTTP__READ_TIMEOUT", "1m")

	var cfg testConfig
	require.NoError(t, config.Load(&cfg,
		config.Bytes("default", []byte(defaultYAML)),
		config.Env("YC_TEST"),
	))
	assert.Equal(t, ":9090", cfg.HTTP.Listen)
	assert.Equal(t, 2048, cfg.HTTP.MaxBodyBytes)
	assert.Equal(t, time.Minute, cfg.HTTP.ReadTimeout)
}

func TestFileOverridesBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "override.yaml")
	require.NoError(t, os.WriteFile(path, []byte("http:\n  listen: \":7070\"\n"), 0o600))

	var cfg testConfig
	require.NoError(t, config.Load(&cfg,
		config.Bytes("default", []byte(defaultYAML)),
		config.File(path),
	))
	assert.Equal(t, ":7070", cfg.HTTP.Listen)
	assert.Equal(t, 5*time.Second, cfg.HTTP.ReadTimeout)
}

func TestOptionalFile(t *testing.T) {
	var cfg testConfig
	require.NoError(t, config.Load(&cfg,
		config.Bytes("default", []byte(defaultYAML)),
		config.OptionalFile(""),
		config.OptionalFile(filepath.Join(t.TempDir(), "missing.yaml")),
	))
	assert.Error(t, config.Load(&cfg, config.File(filepath.Join(t.TempDir(), "missing.yaml"))))
}

func TestValidateRuns(t *testing.T) {
	t.Setenv("YC_TEST_DB_URL", "")
	var cfg testConfig
	err := config.Load(&cfg,
		config.Bytes("default", []byte(defaultYAML+"strict: true\n")),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "database.url is required")
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("YC_SET", "value")
	t.Setenv("YC_EMPTY", "")
	tests := []struct {
		in, want string
	}{
		{"${YC_SET}", "value"},
		{"${YC_UNSET_XYZ}", ""},
		{"${YC_UNSET_XYZ:-fallback}", "fallback"},
		{"${YC_EMPTY:-fallback}", "fallback"},
		{"${YC_SET:-fallback}", "value"},
		{"pa$$word $YC_SET", "pa$$word $YC_SET"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, string(config.ExpandEnv([]byte(tt.in))), tt.in)
	}
}
