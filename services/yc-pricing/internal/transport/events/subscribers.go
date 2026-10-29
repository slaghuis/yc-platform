// Package events holds yc-pricing's event subscribers.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	ycevents "github.com/slaghuis/yc-platform/libs/yc-events"
	"github.com/slaghuis/yc-platform/services/yc-pricing/internal/domain"
)

// Handlers are the use cases the subscribers drive.
type Handlers interface {
	InitShop(ctx context.Context, tenantID string, in domain.ConfigInput) error
	SyncSKU(ctx context.Context, tenantID, skuID, productID, taxClass, status string, at time.Time) error
	DeleteSKUs(ctx context.Context, tenantID string, skuIDs []string, at time.Time) error
}

// Subscription pairs a durable consumer with its handler.
type Subscription struct {
	Opts    eventbus.SubscribeOpts
	Handler eventbus.Handler
}

// Subscriptions are yc-pricing's consumers. SKU events share one consumer
// so a SKU's created, updated and deleted events are applied in order.
func Subscriptions(h Handlers) []Subscription {
	return []Subscription{
		{
			Opts: eventbus.SubscribeOpts{
				Subject:     "yc." + eventbus.PlatformTenantID + "." + ycevents.TypePlatformTenantProvisionedV1,
				DurableName: "yc-pricing-tenant-provisioned",
			},
			Handler: tenantProvisioned(h),
		},
		{
			Opts:    eventbus.SubscribeOpts{Subject: "yc.*.product.sku.*.v1", DurableName: "yc-pricing-sku-changes"},
			Handler: skuChanged(h),
		},
		{
			Opts:    eventbus.SubscribeOpts{Subject: "yc.*." + ycevents.TypeProductDeletedV1, DurableName: "yc-pricing-product-deleted"},
			Handler: productDeleted(h),
		},
	}
}

// RateRefresher fetches exchange rates on demand.
type RateRefresher interface {
	EnsureFresh(ctx context.Context, base string) error
}

// RatesSubscription refreshes a shop's store currency as soon as its config
// is created or changed, rather than at the next scheduled refresh.
func RatesSubscription(r RateRefresher) Subscription {
	return Subscription{
		Opts: eventbus.SubscribeOpts{Subject: "yc.*." + ycevents.TypePricingConfigChangedV1, DurableName: "yc-pricing-rates"},
		Handler: func(ctx context.Context, evt eventbus.ConsumedEvent) error {
			var p ycevents.PricingConfigChangedV1
			if err := decode(evt, &p); err != nil {
				return err
			}
			return r.EnsureFresh(ctx, p.StoreCurrency)
		},
	}
}

func tenantProvisioned(h Handlers) eventbus.Handler {
	return func(ctx context.Context, evt eventbus.ConsumedEvent) error {
		var p ycevents.PlatformTenantProvisionedV1
		if err := decode(evt, &p); err != nil {
			return err
		}
		return poisonInvalid(h.InitShop(ctx, p.TenantID, domain.ConfigInput{
			StoreCurrency: p.StoreCurrency, TaxInclusive: p.IsTaxInclusive, DefaultJurisdiction: p.DefaultJurisdiction,
		}))
	}
}

func skuChanged(h Handlers) eventbus.Handler {
	return func(ctx context.Context, evt eventbus.ConsumedEvent) error {
		tenant := evt.Envelope.TenantID
		switch evt.Envelope.EventType {
		case ycevents.TypeProductSKUCreatedV1:
			var p ycevents.ProductSKUCreatedV1
			if err := decode(evt, &p); err != nil {
				return err
			}
			return h.SyncSKU(ctx, tenant, p.SKUID, p.ProductID, p.TaxClass, p.Status, p.CreatedAt)
		case ycevents.TypeProductSKUUpdatedV1:
			var p ycevents.ProductSKUUpdatedV1
			if err := decode(evt, &p); err != nil {
				return err
			}
			return h.SyncSKU(ctx, tenant, p.SKUID, p.ProductID, p.TaxClass, p.Status, p.UpdatedAt)
		case ycevents.TypeProductSKUDeletedV1:
			var p ycevents.ProductSKUDeletedV1
			if err := decode(evt, &p); err != nil {
				return err
			}
			return h.DeleteSKUs(ctx, tenant, []string{p.SKUID}, p.DeletedAt)
		default:
			return fmt.Errorf("%w: unexpected event type %s", eventbus.ErrPoison, evt.Envelope.EventType)
		}
	}
}

func productDeleted(h Handlers) eventbus.Handler {
	return func(ctx context.Context, evt eventbus.ConsumedEvent) error {
		var p ycevents.ProductDeletedV1
		if err := decode(evt, &p); err != nil {
			return err
		}
		return h.DeleteSKUs(ctx, evt.Envelope.TenantID, p.SKUIds, p.DeletedAt)
	}
}

func decode(evt eventbus.ConsumedEvent, out any) error {
	if err := json.Unmarshal(evt.Payload, out); err != nil {
		return fmt.Errorf("%w: decode payload: %w", eventbus.ErrPoison, err)
	}
	return nil
}

// poisonInvalid sends events with invalid content to the DLQ rather than
// retrying them.
func poisonInvalid(err error) error {
	if ycerrors.HasCode(err, ycerrors.CodeValidationFailed) {
		return fmt.Errorf("%w: %w", eventbus.ErrPoison, err)
	}
	return err
}
