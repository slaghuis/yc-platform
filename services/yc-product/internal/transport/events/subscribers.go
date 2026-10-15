// Package events holds yc-product's event subscribers, which feed the
// search projection.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	ycerrors "github.com/slaghuis/yc-platform/libs/yc-errors"
	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
	ycevents "github.com/slaghuis/yc-platform/libs/yc-events"
	"github.com/slaghuis/yc-platform/services/yc-product/internal/domain"
)

// Handlers are the use cases the subscribers drive.
type Handlers interface {
	RecordOrder(ctx context.Context, tenantID, userID string, customer bool, skuIDs []string, at time.Time) error
	SetReviewStats(ctx context.Context, tenantID, productID string, stats domain.ReviewStats) error
	SetBasePrice(ctx context.Context, tenantID, skuID string, minor *int64, at time.Time) error
	SetInStock(ctx context.Context, tenantID, skuID string, inStock bool, at time.Time) error
	ForgetUser(ctx context.Context, tenantID string, e ycevents.IdentityUserDeletedV1) error
}

// Subscription pairs a durable consumer with its handler.
type Subscription struct {
	Opts    eventbus.SubscribeOpts
	Handler eventbus.Handler
}

// reviewStats is the part of review.* payloads yc-product projects.
type reviewStats struct {
	ProductID    string                      `json:"product_id"`
	ProductStats ycevents.ProductReviewStats `json:"product_stats"`
}

// reviewEvents carry the product's stats after the change.
var reviewEvents = map[string]string{
	ycevents.TypeReviewSubmittedV1:   "yc-product-review-submitted",
	ycevents.TypeReviewEditedV1:      "yc-product-review-edited",
	ycevents.TypeReviewApprovedV1:    "yc-product-review-approved",
	ycevents.TypeReviewRejectedV1:    "yc-product-review-rejected",
	ycevents.TypeReviewDeletedV1:     "yc-product-review-deleted",
	ycevents.TypeReviewLikeAddedV1:   "yc-product-review-like-added",
	ycevents.TypeReviewLikeRemovedV1: "yc-product-review-like-removed",
}

// Subscriptions are yc-product's consumers.
func Subscriptions(h Handlers) []Subscription {
	gdprOpts, gdpr := ycevents.OnUserDeleted("yc-product-gdpr", func(ctx context.Context, tenantID string, e ycevents.IdentityUserDeletedV1) error {
		return poisonInvalid(h.ForgetUser(ctx, tenantID, e))
	})
	return append([]Subscription{
		{Opts: gdprOpts, Handler: gdpr},
		{
			Opts: eventbus.SubscribeOpts{
				Subject:     "yc.*." + ycevents.TypeOrderPlacedV1,
				DurableName: "yc-product-order-placed",
			},
			Handler: func(ctx context.Context, evt eventbus.ConsumedEvent) error {
				var p ycevents.OrderPlacedV1
				if err := decode(evt, &p); err != nil {
					return err
				}
				skus := make([]string, len(p.Items))
				for i, it := range p.Items {
					skus[i] = it.SKUID
				}
				return poisonInvalid(h.RecordOrder(ctx, evt.Envelope.TenantID, p.UserID, p.UserType == "customer",
					skus, setAt(evt, p.PlacedAt)))
			},
		},
		{
			Opts: eventbus.SubscribeOpts{
				Subject:     "yc.*." + ycevents.TypePricingBasePriceSetV1,
				DurableName: "yc-product-base-price-set",
			},
			Handler: func(ctx context.Context, evt eventbus.ConsumedEvent) error {
				var p ycevents.PricingBasePriceSetV1
				if err := decode(evt, &p); err != nil {
					return err
				}
				minor := p.Price.AmountMinor
				return poisonInvalid(h.SetBasePrice(ctx, evt.Envelope.TenantID, p.SKUID, &minor, setAt(evt, p.ValidFrom)))
			},
		},
		{
			Opts: eventbus.SubscribeOpts{
				Subject:     "yc.*." + ycevents.TypePricingBasePriceRemovedV1,
				DurableName: "yc-product-base-price-removed",
			},
			Handler: func(ctx context.Context, evt eventbus.ConsumedEvent) error {
				var p ycevents.PricingBasePriceRemovedV1
				if err := decode(evt, &p); err != nil {
					return err
				}
				return poisonInvalid(h.SetBasePrice(ctx, evt.Envelope.TenantID, p.SKUID, nil, setAt(evt, p.RemovedAt)))
			},
		},
		{
			Opts: eventbus.SubscribeOpts{
				Subject:     "yc.*." + ycevents.TypeInventoryStockDepletedV1,
				DurableName: "yc-product-stock-depleted",
			},
			Handler: func(ctx context.Context, evt eventbus.ConsumedEvent) error {
				var p ycevents.InventoryStockDepletedV1
				if err := decode(evt, &p); err != nil {
					return err
				}
				return poisonInvalid(h.SetInStock(ctx, evt.Envelope.TenantID, p.SKUID, false, setAt(evt, p.DepletedAt)))
			},
		},
		{
			Opts: eventbus.SubscribeOpts{
				Subject:     "yc.*." + ycevents.TypeInventoryStockRestoredV1,
				DurableName: "yc-product-stock-restored",
			},
			Handler: func(ctx context.Context, evt eventbus.ConsumedEvent) error {
				var p ycevents.InventoryStockRestoredV1
				if err := decode(evt, &p); err != nil {
					return err
				}
				return poisonInvalid(h.SetInStock(ctx, evt.Envelope.TenantID, p.SKUID, true, setAt(evt, p.RestoredAt)))
			},
		},
	}, reviewSubscriptions(h)...)
}

func reviewSubscriptions(h Handlers) []Subscription {
	subs := make([]Subscription, 0, len(reviewEvents))
	for eventType, durable := range reviewEvents {
		subs = append(subs, Subscription{
			Opts: eventbus.SubscribeOpts{Subject: "yc.*." + eventType, DurableName: durable},
			Handler: func(ctx context.Context, evt eventbus.ConsumedEvent) error {
				var p reviewStats
				if err := decode(evt, &p); err != nil {
					return err
				}
				s := p.ProductStats
				return poisonInvalid(h.SetReviewStats(ctx, evt.Envelope.TenantID, p.ProductID, domain.ReviewStats{
					RatingCount: s.RatingCount, RatingSum: s.RatingSum, LikeCount: s.LikeCount, Version: s.Version,
				}))
			},
		})
	}
	return subs
}

// setAt orders price and stock events: the payload's time, else when the
// event occurred.
func setAt(evt eventbus.ConsumedEvent, at time.Time) time.Time {
	if at.IsZero() {
		return evt.Envelope.EventTime
	}
	return at
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
