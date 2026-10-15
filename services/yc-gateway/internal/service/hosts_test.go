package service_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/slaghuis/yc-platform/services/yc-gateway/internal/service"
)

func TestPlatformHosts(t *testing.T) {
	h, err := service.NewHosts(nil, map[string]string{"shop-a.test": "shop-a"}, []string{"Admin.Test"})
	require.NoError(t, err)

	tenant, ok := h.Resolve("admin.test:8090")
	assert.True(t, ok)
	assert.Equal(t, "_platform", tenant)

	_, err = service.NewHosts(nil, map[string]string{"admin.test": "shop-a"}, []string{"admin.test"})
	require.ErrorContains(t, err, "also a platform host")

	_, err = service.NewHosts(nil, map[string]string{"x.test": "_platform"}, nil)
	require.Error(t, err, "tenant_hosts cannot map to _platform")

	err = h.Provision(t.Context(), "shop-b", []string{"admin.test"})
	require.ErrorContains(t, err, "is _platform in config")
}
