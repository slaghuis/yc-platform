package httpkit

import (
	"context"
	"net/http"
	"os"
	"time"

	telemetry "github.com/slaghuis/yc-platform/libs/yc-telemetry"
)

// Registration is the body of yc-gateway's POST /internal/register
// (ADR-020 rule 7). It replaces the service's previous registration.
type Registration struct {
	ServiceName string            `json:"service_name"`
	InstanceID  string            `json:"instance_id"`
	BaseURL     string            `json:"base_url"`
	Version     string            `json:"version,omitempty"`
	HealthURL   string            `json:"health_url"` // path under BaseURL
	Routes      []RegisteredRoute `json:"routes"`
}

// RegisteredRoute is one route in a Registration; the fields mean what
// they mean in yc-gateway's static routes.
type RegisteredRoute struct {
	Prefix  string   `json:"prefix"`
	Methods []string `json:"methods,omitempty"`
	Roles   []string `json:"roles,omitempty"`
	// Anonymous routes are reachable without a token on platform hosts.
	Anonymous bool `json:"anonymous,omitempty"`
	// Callback routes take third-party callbacks without a token (payment
	// webhooks): the gateway forwards them with the host's tenant and no
	// actor, and the service verifies them itself.
	Callback bool `json:"callback,omitempty"`
}

// Deregistration is the body of POST /internal/deregister.
type Deregistration struct {
	ServiceName string `json:"service_name"`
	InstanceID  string `json:"instance_id"`
}

// GatewayConfig is a service's `gateway:` block. An empty InternalURL
// disables registration; the gateway's static routes still apply.
type GatewayConfig struct {
	// InternalURL is the gateway's internal listener.
	InternalURL string `koanf:"internal_url"`
	// AdvertiseURL is this service's base URL as the gateway reaches it.
	AdvertiseURL string        `koanf:"advertise_url"`
	Interval     time.Duration `koanf:"interval"`
}

// KeepRegistered registers reg with the gateway, then re-registers every
// cfg.Interval (5 s by default) so a restarted gateway learns the routes
// again within that. An unchanged re-registration costs the gateway a map
// lookup. When ctx ends it deregisters. It blocks until ctx ends.
func KeepRegistered(ctx context.Context, cfg GatewayConfig, reg Registration) {
	if cfg.InternalURL == "" {
		return
	}
	logger := telemetry.LoggerFromContext(ctx)
	if reg.InstanceID == "" {
		reg.InstanceID, _ = os.Hostname()
	}
	if reg.BaseURL == "" {
		reg.BaseURL = cfg.AdvertiseURL
	}
	if reg.HealthURL == "" {
		reg.HealthURL = pathHealthz
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	client := NewClient(ClientOptions{Timeout: 5 * time.Second})

	registered := false
	retry := time.Second
	for {
		err := DoJSON(ctx, client, http.MethodPost, cfg.InternalURL+"/internal/register", reg, nil)
		wait := interval
		switch {
		case err == nil:
			if !registered {
				logger.Info("registered with gateway", "gateway", cfg.InternalURL, "routes", len(reg.Routes))
			}
			registered, retry = true, time.Second
		case ctx.Err() == nil:
			logger.Warn("gateway registration failed", "gateway", cfg.InternalURL, "err", err)
			registered, wait = false, retry
			retry = min(retry*2, interval)
		}
		select {
		case <-ctx.Done():
			if registered {
				dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
				err := DoJSON(dctx, client, http.MethodPost, cfg.InternalURL+"/internal/deregister",
					Deregistration{ServiceName: reg.ServiceName, InstanceID: reg.InstanceID}, nil)
				cancel()
				if err != nil {
					logger.Warn("gateway deregistration failed", "err", err)
				}
			}
			return
		case <-time.After(wait):
		}
	}
}
