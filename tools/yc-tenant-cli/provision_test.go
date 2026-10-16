package main

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseProvision(t *testing.T) {
	o, err := parseProvision([]string{
		"--token", "tok",
		"--tenant-id", "shop-abc", "--name", "Shop ABC",
		"--hostname", "Shop-ABC.localhost", "--hostname", "www.shop-abc.localhost",
		"--primary-admin-email", "admin@shop-abc.test",
	}, io.Discard)
	require.NoError(t, err)

	assert.Equal(t, "shop-abc", o.body.TenantID)
	assert.Equal(t, []string{"shop-abc.localhost", "www.shop-abc.localhost"}, o.body.Hostnames)
	assert.Equal(t, "ZAR", o.body.StoreCurrency)
	assert.Equal(t, "ZA", o.body.DefaultJurisdiction)
	assert.True(t, o.body.IsTaxInclusive)
	assert.Equal(t, "admin@shop-abc.test", o.body.ContactEmail)
	require.NotNil(t, o.body.PrimaryAdminEmail)
	assert.Equal(t, "admin@shop-abc.test", *o.body.PrimaryAdminEmail)
	assert.Equal(t, "http://admin.localhost:8070", o.gatewayURL)
}

func TestParseProvisionRejects(t *testing.T) {
	for name, args := range map[string][]string{
		"no token":        {"--tenant-id", "shop-a", "--name", "x", "--hostname", "a.test", "--contact-email", "a@b.c"},
		"no tenant":       {"--token", "t", "--name", "x", "--hostname", "a.test", "--contact-email", "a@b.c"},
		"platform tenant": {"--token", "t", "--tenant-id", "_platform", "--name", "x", "--hostname", "a.test", "--contact-email", "a@b.c"},
		"bad tenant":      {"--token", "t", "--tenant-id", "Shop_ABC", "--name", "x", "--hostname", "a.test", "--contact-email", "a@b.c"},
		"no name":         {"--token", "t", "--tenant-id", "shop-a", "--hostname", "a.test", "--contact-email", "a@b.c"},
		"no hostname":     {"--token", "t", "--tenant-id", "shop-a", "--name", "x", "--contact-email", "a@b.c"},
		"no contact":      {"--token", "t", "--tenant-id", "shop-a", "--name", "x", "--hostname", "a.test"},
		"stray argument":  {"--token", "t", "--tenant-id", "shop-a", "--name", "x", "--hostname", "a.test", "--contact-email", "a@b.c", "extra"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseProvision(args, io.Discard)
			assert.Error(t, err)
		})
	}
}

func TestRunUnknownCommand(t *testing.T) {
	assert.Error(t, run(context.Background(), []string{"explode"}, io.Discard, io.Discard))
	assert.Error(t, run(context.Background(), nil, io.Discard, io.Discard))
	assert.NoError(t, run(context.Background(), []string{"help"}, io.Discard, io.Discard))
}
