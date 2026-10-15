package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/slaghuis/yc-platform/services/yc-gateway/internal/service"
)

func TestRegistryReadyAfterStartupGrace(t *testing.T) {
	r, err := service.NewRegistry(nil, nil, nil, service.RegistryConfig{
		HealthInterval: time.Second, HealthTimeout: time.Second, FailureThreshold: 1,
		StartupGrace: 200 * time.Millisecond,
	})
	require.NoError(t, err)

	assert.Error(t, r.Ready(context.Background()), "unready while services re-register")
	assert.Eventually(t, func() bool { return r.Ready(context.Background()) == nil },
		2*time.Second, 20*time.Millisecond)
}
