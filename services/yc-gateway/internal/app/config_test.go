package app

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListsAcceptCommaSeparatedEnvironmentValues(t *testing.T) {
	c := Config{
		PlatformHosts:  []string{"yellowcommerce.co.za, www.yellowcommerce.co.za"},
		TrustedProxies: []string{"10.0.0.0/8,172.16.0.0/12,,192.168.0.1"},
	}

	assert.Equal(t, []string{"yellowcommerce.co.za", "www.yellowcommerce.co.za"}, c.platformHosts())
	proxies, err := c.trustedProxies()
	require.NoError(t, err)
	assert.Equal(t, []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.1/32"),
	}, proxies)
}
