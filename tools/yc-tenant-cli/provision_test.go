package main

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	events "github.com/slaghuis/yc-platform/libs/yc-events"
)

func TestParseProvision(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.FixedZone("SAST", 2*3600))
	o, err := parseProvision([]string{
		"--tenant-id", "shop-abc", "--name", "Shop ABC",
		"--hostname", "Shop-ABC.localhost", "--hostname", "www.shop-abc.localhost",
		"--primary-admin-email", "admin@shop-abc.test",
	}, io.Discard, now)
	require.NoError(t, err)

	p := o.payload
	assert.Equal(t, "shop-abc", p.TenantID)
	assert.Equal(t, []string{"shop-abc.localhost", "www.shop-abc.localhost"}, p.Hostnames)
	assert.Equal(t, "ZAR", p.StoreCurrency)
	assert.Equal(t, "ZA", p.DefaultJurisdiction)
	assert.True(t, p.IsTaxInclusive)
	require.NotNil(t, p.PrimaryAdminEmail)
	assert.Equal(t, time.UTC, p.ProvisionedAt.Location())

	// The payload satisfies the schema the consumers validate against.
	v, err := events.NewValidator()
	require.NoError(t, err)
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	require.NoError(t, v.ValidatePayload(context.Background(), &eventbus.Envelope{
		EventType: events.TypePlatformTenantProvisionedV1, Payload: raw,
	}))
}

func TestParseProvisionRejects(t *testing.T) {
	for name, args := range map[string][]string{
		"no tenant":       {"--name", "x", "--hostname", "a.test"},
		"platform tenant": {"--tenant-id", "_platform", "--name", "x", "--hostname", "a.test"},
		"bad tenant":      {"--tenant-id", "Shop_ABC", "--name", "x", "--hostname", "a.test"},
		"no name":         {"--tenant-id", "shop-a", "--hostname", "a.test"},
		"no hostname":     {"--tenant-id", "shop-a", "--name", "x"},
		"stray argument":  {"--tenant-id", "shop-a", "--name", "x", "--hostname", "a.test", "extra"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseProvision(args, io.Discard, time.Now())
			assert.Error(t, err)
		})
	}
}

func TestRunUnknownCommand(t *testing.T) {
	assert.Error(t, run(context.Background(), []string{"explode"}, io.Discard, io.Discard))
	assert.Error(t, run(context.Background(), nil, io.Discard, io.Discard))
	assert.NoError(t, run(context.Background(), []string{"help"}, io.Discard, io.Discard))
}
