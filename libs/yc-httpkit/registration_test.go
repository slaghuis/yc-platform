package httpkit_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	httpkit "github.com/slaghuis/yc-platform/libs/yc-httpkit"
	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
)

func TestKeepRegistered(t *testing.T) {
	telemetry.InitForTest(t, "yc-test")
	var mu sync.Mutex
	var calls []string
	var first httpkit.Registration
	failures := 1
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/internal/register" && failures > 0 {
			failures--
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if len(calls) == 0 {
			_ = json.NewDecoder(r.Body).Decode(&first)
		}
		calls = append(calls, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer gw.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		httpkit.KeepRegistered(ctx, httpkit.GatewayConfig{
			InternalURL: gw.URL, AdvertiseURL: "http://svc:8080", Interval: 50 * time.Millisecond,
		}, httpkit.Registration{ServiceName: "yc-test", Routes: []httpkit.RegisteredRoute{{Prefix: "/x"}}})
		close(done)
	}()

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(calls) >= 2
	}, 5*time.Second, 10*time.Millisecond, "retries after a failure, then re-registers")
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "/internal/deregister", calls[len(calls)-1])
	assert.Equal(t, "http://svc:8080", first.BaseURL)
	assert.Equal(t, "/healthz", first.HealthURL)
	assert.NotEmpty(t, first.InstanceID)
}

func TestKeepRegisteredDisabled(t *testing.T) {
	httpkit.KeepRegistered(context.Background(), httpkit.GatewayConfig{}, httpkit.Registration{})
}
