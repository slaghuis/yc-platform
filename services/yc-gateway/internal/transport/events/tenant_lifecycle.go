package events

import (
	"context"
	"encoding/json"
	"fmt"

	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	ycevents "github.com/slaghuis/yc-platform/libs/yc-events"
)

// Lifecycle applies tenant hostname and suspension changes from yc-tenant.
type Lifecycle interface {
	Provision(ctx context.Context, tenantID string, hostnames []string) error
	ReplaceHostnames(ctx context.Context, tenantID string, hostnames []string) error
	Suspend(ctx context.Context, tenantID, reason string) error
	Reinstate(ctx context.Context, tenantID string) error
	Delete(ctx context.Context, tenantID string) error
}

// TenantUpdatedSubscription is the durable consumer for platform.tenant.updated.v1.
var TenantUpdatedSubscription = eventbus.SubscribeOpts{
	Subject:     "yc." + eventbus.PlatformTenantID + "." + ycevents.TypePlatformTenantUpdatedV1,
	DurableName: "yc-gateway-tenant-updated",
}

// TenantSuspendedSubscription is the durable consumer for platform.tenant.suspended.v1.
var TenantSuspendedSubscription = eventbus.SubscribeOpts{
	Subject:     "yc." + eventbus.PlatformTenantID + "." + ycevents.TypePlatformTenantSuspendedV1,
	DurableName: "yc-gateway-tenant-suspended",
}

// TenantReinstatedSubscription is the durable consumer for platform.tenant.reinstated.v1.
var TenantReinstatedSubscription = eventbus.SubscribeOpts{
	Subject:     "yc." + eventbus.PlatformTenantID + "." + ycevents.TypePlatformTenantReinstatedV1,
	DurableName: "yc-gateway-tenant-reinstated",
}

// TenantDeletedSubscription is the durable consumer for platform.tenant.deleted.v1.
var TenantDeletedSubscription = eventbus.SubscribeOpts{
	Subject:     "yc." + eventbus.PlatformTenantID + "." + ycevents.TypePlatformTenantDeletedV1,
	DurableName: "yc-gateway-tenant-deleted",
}

// TenantUpdated replaces a shop's hostname set.
func TenantUpdated(l Lifecycle) eventbus.Handler {
	return func(ctx context.Context, evt eventbus.ConsumedEvent) error {
		var payload ycevents.PlatformTenantUpdatedV1
		if err := json.Unmarshal(evt.Payload, &payload); err != nil {
			return fmt.Errorf("%w: decode payload: %w", eventbus.ErrPoison, err)
		}
		return l.ReplaceHostnames(ctx, payload.TenantID, payload.Hostnames)
	}
}

// TenantSuspended blocks traffic for the shop.
func TenantSuspended(l Lifecycle) eventbus.Handler {
	return func(ctx context.Context, evt eventbus.ConsumedEvent) error {
		var payload ycevents.PlatformTenantSuspendedV1
		if err := json.Unmarshal(evt.Payload, &payload); err != nil {
			return fmt.Errorf("%w: decode payload: %w", eventbus.ErrPoison, err)
		}
		return l.Suspend(ctx, payload.TenantID, payload.Reason)
	}
}

// TenantReinstated clears a suspension.
func TenantReinstated(l Lifecycle) eventbus.Handler {
	return func(ctx context.Context, evt eventbus.ConsumedEvent) error {
		var payload ycevents.PlatformTenantReinstatedV1
		if err := json.Unmarshal(evt.Payload, &payload); err != nil {
			return fmt.Errorf("%w: decode payload: %w", eventbus.ErrPoison, err)
		}
		return l.Reinstate(ctx, payload.TenantID)
	}
}

// TenantDeleted frees hostnames and answers 410 until they are reclaimed.
func TenantDeleted(l Lifecycle) eventbus.Handler {
	return func(ctx context.Context, evt eventbus.ConsumedEvent) error {
		var payload ycevents.PlatformTenantDeletedV1
		if err := json.Unmarshal(evt.Payload, &payload); err != nil {
			return fmt.Errorf("%w: decode payload: %w", eventbus.ErrPoison, err)
		}
		return l.Delete(ctx, payload.TenantID)
	}
}
