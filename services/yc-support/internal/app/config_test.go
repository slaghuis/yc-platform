package app_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/slaghuis/yc-platform/services/yc-support/internal/app"
)

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("YC_SUPPORT_DB_URL", "postgres://u:p@localhost/yc_support")
	t.Setenv("NATS_URL", "nats://localhost:4222")
	t.Setenv("YC_GATEWAY_INTERNAL_URL", "http://gateway:9090")
	t.Setenv("YC_SUPPORT_SERVICE_SECRET", "secret")
}

func TestLoadConfigDefaults(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := app.LoadConfig()
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())
	assert.Equal(t, "yc-support", cfg.Service)
	assert.Equal(t, 72*time.Hour, cfg.Support.IdleTTL)
	assert.Equal(t, time.Minute, cfg.Support.SweepInterval)
	assert.Equal(t, 50, cfg.Support.SweepBatch)
	assert.Empty(t, cfg.Support.StorefrontBase, "relative thread links by default")
	assert.Equal(t, 5*time.Minute, cfg.Tenant.ContactCacheTTL)
}

func TestLoadConfigOverrides(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("YC_SUPPORT_IDLE_TTL", "24h")
	t.Setenv("YC_SUPPORT_SWEEP_INTERVAL", "30s")
	t.Setenv("YC_SUPPORT_STOREFRONT_BASE", "https://{tenant_id}.shops.test")
	cfg, err := app.LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, 24*time.Hour, cfg.Support.IdleTTL)
	assert.Equal(t, 30*time.Second, cfg.Support.SweepInterval)
	assert.Equal(t, "https://{tenant_id}.shops.test", cfg.Support.StorefrontBase)
}

func TestLoadConfigNeedsItsEnvironment(t *testing.T) {
	t.Setenv("YC_SUPPORT_DB_URL", "")
	t.Setenv("NATS_URL", "")
	t.Setenv("YC_GATEWAY_INTERNAL_URL", "")
	t.Setenv("YC_SUPPORT_SERVICE_SECRET", "")
	_, err := app.LoadConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "YC_SUPPORT_DB_URL")
}

func TestValidateReportsEverything(t *testing.T) {
	cfg := app.Config{Support: app.SupportConfig{IdleTTL: time.Hour, SweepInterval: time.Minute, SweepBatch: 1}}
	err := cfg.Validate()
	require.Error(t, err)
	for _, want := range []string{"database.url", "nats.url", "internal_calls.gateway_url", "internal_calls.client_secret"} {
		assert.Contains(t, err.Error(), want)
	}
}

func TestValidateRejectsBadTimings(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := app.LoadConfig()
	require.NoError(t, err)
	cfg.Support.IdleTTL, cfg.Support.SweepInterval, cfg.Support.SweepBatch = 0, -time.Second, 0
	err = cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "idle_ttl")
	assert.Contains(t, err.Error(), "sweep_interval")
	assert.Contains(t, err.Error(), "sweep_batch")
}
