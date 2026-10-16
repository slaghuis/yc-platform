// Package events holds yc-gateway's event subscribers.
package events

import (
	"context"
	"encoding/json"
	"fmt"

	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	ycevents "github.com/slaghuis/yc-platform/libs/yc-events"
)

// TenantProvisionedSubscription is the durable consumer for
// platform.tenant.provisioned.v1, which is only published on _platform.
var TenantProvisionedSubscription = eventbus.SubscribeOpts{
	Subject:     "yc." + eventbus.PlatformTenantID + "." + ycevents.TypePlatformTenantProvisionedV1,
	DurableName: "yc-gateway-tenant-provisioned",
}

// TenantProvisioned maps the new tenant's hostnames.
func TenantProvisioned(l Lifecycle) eventbus.Handler {
	return func(ctx context.Context, evt eventbus.ConsumedEvent) error {
		var payload ycevents.PlatformTenantProvisionedV1
		if err := json.Unmarshal(evt.Payload, &payload); err != nil {
			return fmt.Errorf("%w: decode payload: %w", eventbus.ErrPoison, err)
		}
		return l.Provision(ctx, payload.TenantID, payload.Hostnames)
	}
}
