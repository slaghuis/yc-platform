# Event Taxonomy

Registry of every event in yc-platform: who produces it, who consumes it, and what triggers it.

Producers and consumers are taken from the **Published Events** and **Subscribed Events** tables in
`docs/services/*.md`; the specs are authoritative. Platform tenant events come from `yc-tenant`, which
has no spec. When you add or change an event, update the producing and consuming specs and this registry
in the same PR (`docs/onboarding/adding-an-event.md`).

Event types follow `<domain>[.<entity>].<action>.v<N>` (ADR-001). The bus carries facts only; the checkout
saga uses synchronous calls, not command events (ADR-019).

Consumers listed here subscribe to the exact event type. Pattern subscriptions are listed at the end.

## Platform Domain

| Event Type | Producer | Consumers | Trigger |
|---|---|---|---|
| `platform.service.registered.v1` | yc-gateway | yc-monitor | A backend service registers |
| `platform.service.deregistered.v1` | yc-gateway | yc-monitor | A backend service deregisters or fails health |
| `platform.tenant.provisioned.v1` | yc-tenant | yc-analytics, yc-audit, yc-cart, yc-checkout, yc-gateway, yc-identity, yc-inventory, yc-message, yc-orders, yc-payment, yc-preference, yc-pricing, yc-review, yc-shipping | System admin creates a shop (ADR-029) |
| `platform.tenant.updated.v1` | yc-tenant | yc-analytics, yc-gateway, yc-message | System admin renames a shop or replaces its hostnames |
| `platform.tenant.suspended.v1` | yc-tenant | yc-gateway | System admin suspends a shop |
| `platform.tenant.reinstated.v1` | yc-tenant | yc-gateway | System admin reinstates a suspended shop |
| `platform.tenant.deleted.v1` | yc-tenant | yc-gateway | System admin deletes a suspended shop (data purge is backlog-v2) |
| `platform.gdpr.cascade_completed.v1` | Every GDPR cascade participant (ADR-005, ADR-025) | yc-audit | Service completes its part of a GDPR deletion (`deleted`, `anonymized` or `pseudonymized`, with a count) |

## Identity Domain

| Event Type | Producer | Consumers | Trigger |
|---|---|---|---|
| `identity.visitor.created.v1` | yc-identity | yc-analytics, yc-monitor | Visitor token issued |
| `identity.user.registered.v1` | yc-identity | yc-message, yc-orders, yc-preference | New customer registration |
| `identity.user.promoted.v1` | yc-identity | yc-audit, yc-message | Role changed upward (yc-message keeps its shop admins) |
| `identity.user.demoted.v1` | yc-identity | yc-audit, yc-message | Shop admin demoted to customer (ADR-030) |
| `identity.user.invited.v1` | yc-identity | yc-audit | System admin invited an email to a role; invite link sent via `POST /internal/send` `admin_invite` (ADR-030) |
| `identity.user.invitation_accepted.v1` | yc-identity | yc-audit, yc-message, yc-preference | Invitation accepted; admin audience and preference defaults (ADR-030) |
| `identity.user.invitation_revoked.v1` | yc-identity | yc-audit | Pending invitation revoked (ADR-030) |
| `identity.user.logged_in.v1` | yc-identity | yc-cart | Successful login |
| `identity.user.login_failed.v1` | yc-identity | yc-audit | Failed login (for brute-force detection) |
| `identity.user.password_reset_requested.v1` | yc-identity | — | Reset flow started (no token; yc-identity sends the link through yc-message `POST /internal/send`, ADR-023) |
| `identity.user.password_changed.v1` | yc-identity | — | Password successfully changed |
| `identity.user.suspended.v1` | yc-identity | yc-audit, yc-message | Admin-suspended; leaves alert audiences (ADR-030) |
| `identity.user.reactivated.v1` | yc-identity | yc-audit, yc-message | Suspended user reactivated (ADR-030) |
| `identity.user.deleted.v1` | yc-identity | yc-analytics, yc-audit, yc-cart, yc-checkout, yc-message, yc-orders, yc-payment, yc-preference, yc-product, yc-review, yc-shipping, yc-support | `DELETE /me` or `DELETE /admin/users/{id}`; carries the pseudonym and kicks off the GDPR cascade (yc-inventory joins with back-in-stock subscriptions) |
| `identity.oauth_linked.v1` | yc-identity | — | OAuth provider linked to user |
| `identity.oauth_unlinked.v1` | yc-identity | — | OAuth provider unlinked |

## Product Domain

| Event Type | Producer | Consumers | Trigger |
|---|---|---|---|
| `product.supplier.created.v1` | yc-product | — | Supplier created |
| `product.supplier.updated.v1` | yc-product | — | Supplier updated |
| `product.supplier.deleted.v1` | yc-product | — | Supplier deleted |
| `product.created.v1` | yc-product | yc-analytics | Product created |
| `product.updated.v1` | yc-product | yc-analytics | Product updated |
| `product.deleted.v1` | yc-product | yc-cart, yc-image, yc-pricing, yc-review | Product soft-deleted |
| `product.sku.created.v1` | yc-product | yc-analytics, yc-inventory, yc-pricing | SKU created (carries `inventory.tracked`, `tax_class`) |
| `product.sku.updated.v1` | yc-product | yc-analytics, yc-cart, yc-inventory, yc-pricing | SKU updated |
| `product.sku.deleted.v1` | yc-product | yc-cart, yc-image, yc-inventory, yc-pricing | SKU soft-deleted |
| `product.variant.added.v1` | yc-product | — | Variant axis added |
| `product.variant.removed.v1` | yc-product | — | Variant axis removed |
| `product.image_attached.v1` | yc-product | — | Image linked to product/SKU |
| `product.image_detached.v1` | yc-product | — | Image unlinked |
| `product.category.created.v1` | yc-product | — | Category created (superseded by v2) |
| `product.category.updated.v1` | yc-product | — | Category updated (superseded by v2) |
| `product.category.created.v2` | yc-product | — | Category created, including the optional icon |
| `product.category.updated.v2` | yc-product | — | Category updated, including the optional icon |
| `product.category.deleted.v1` | yc-product | — | Category deleted |
| `product.banner.created.v1` | yc-product | — | Banner created |
| `product.banner.updated.v1` | yc-product | — | Banner updated |
| `product.banner.deleted.v1` | yc-product | — | Banner deleted, or removed with its category |
| `product.banners.reordered.v1` | yc-product | — | Banner display order changed |

## Pricing Domain

| Event Type | Producer | Consumers | Trigger |
|---|---|---|---|
| `pricing.base_price.set.v1` | yc-pricing | yc-cart, yc-product | Base price created or changed |
| `pricing.base_price.removed.v1` | yc-pricing | yc-product | Base price removed |
| `pricing.tax_class.created.v1` | yc-pricing | — | New tax class defined |
| `pricing.tax_class.updated.v1` | yc-pricing | — | Tax class rules changed |
| `pricing.tax_class.deleted.v1` | yc-pricing | — | Tax class removed |
| `pricing.promotion.created.v1` | yc-pricing | — | Promotion defined |
| `pricing.promotion.started.v1` | yc-pricing | yc-cart, yc-product | Promotion becomes active (scheduled or manual) |
| `pricing.promotion.ended.v1` | yc-pricing | yc-product | Promotion ends |
| `pricing.promotion.updated.v1` | yc-pricing | — | Promotion changed |
| `pricing.promotion.deleted.v1` | yc-pricing | — | Promotion removed |
| `pricing.config.changed.v1` | yc-pricing | yc-audit, yc-pricing (fetches rates for a new store currency) | Tenant pricing config created or changed |
| `pricing.exchange_rates_updated.v1` | yc-pricing | — | Rate cache refreshed |
| `pricing.exchange_rates_stale.v1` | yc-pricing | yc-monitor | Rate fetch has failed for > 24h |

## Inventory Domain

| Event Type | Producer | Consumers | Trigger |
|---|---|---|---|
| `inventory.stock.received.v1` | yc-inventory | — | Stock received by admin |
| `inventory.stock.adjusted.v1` | yc-inventory | — | Admin adjusted stock (shrinkage, correction) |
| `inventory.stock.reserved.v1` | yc-inventory | — | Reservation created |
| `inventory.stock.reservation_failed.v1` | yc-inventory | — | Reservation declined (shortage) |
| `inventory.stock.reservation_released.v1` | yc-inventory | — | Reservation released (compensation or admin) |
| `inventory.stock.reservation_expired.v1` | yc-inventory | yc-checkout | Reservation timed out |
| `inventory.stock.committed.v1` | yc-inventory | — | Reservation committed to order |
| `inventory.stock.depleted.v1` | yc-inventory | yc-cart, yc-product | Available dropped to 0 |
| `inventory.stock.restored.v1` | yc-inventory | yc-cart, yc-product | Available rose above 0 |
| `inventory.stock.low_threshold_reached.v1` | yc-inventory | yc-message | Available crossed below threshold |
| `inventory.back_in_stock.subscribed.v1` | yc-inventory | — | Customer subscribed to a SKU |
| `inventory.back_in_stock.notified.v1` | yc-inventory | yc-message | Notification issued (consumed by yc-message) |

## Cart Domain

| Event Type | Producer | Consumers | Trigger |
|---|---|---|---|
| `cart.created.v1` | yc-cart | yc-analytics | New cart (first add) |
| `cart.item.added.v1` | yc-cart | yc-analytics | Item added |
| `cart.item.removed.v1` | yc-cart | — | Item removed |
| `cart.item.quantity_changed.v1` | yc-cart | — | Quantity updated |
| `cart.item.moved.v1` | yc-cart | — | Item moved between carts |
| `cart.item.expired.v1` | yc-cart | — | Item removed by expiry sweep |
| `cart.cleared.v1` | yc-cart | yc-analytics | Cart cleared (manually or post-checkout) |
| `cart.merged.v1` | yc-cart | — | Visitor cart merged into customer cart |
| `cart.abandoned.v1` | yc-cart | yc-analytics, yc-message | Idle beyond threshold |
| `cart.converted.v1` | yc-cart | yc-analytics | Cart handed off to checkout successfully |

## Preference Domain

| Event Type | Producer | Consumers | Trigger |
|---|---|---|---|
| `preference.currency.changed.v1` | yc-preference | yc-cart | Display currency changed |
| `preference.channel.added.v1` | yc-preference | yc-message | New messaging channel added |
| `preference.channel.changed.v1` | yc-preference | — | Primary channel changed |
| `preference.channel.verified.v1` | yc-preference | — | A channel was verified |
| `preference.channel.removed.v1` | yc-preference | — | Channel removed |
| `preference.facet.updated.v1` | yc-preference | — | Preferred facets changed |
| `preference.locale.changed.v1` | yc-preference | — | Locale changed |

## Review Domain

| Event Type | Producer | Consumers | Trigger |
|---|---|---|---|
| `review.submitted.v1` | yc-review | yc-analytics, yc-product | New review (goes to pending or auto-approved); review and like events carry the product's versioned stats (ADR-024) |
| `review.approved.v1` | yc-review | yc-analytics, yc-product | Review approved |
| `review.rejected.v1` | yc-review | yc-analytics, yc-image, yc-product | Review rejected (by a moderator, or no verified purchase) |
| `review.edited.v1` | yc-review | yc-product | Author edited (re-enters pending under manual moderation) |
| `review.deleted.v1` | yc-review | yc-product | Author or admin deleted |
| `review.responded.v1` | yc-review | — | Shop admin added response |
| `review.like.added.v1` | yc-review | yc-product | Product liked |
| `review.like.removed.v1` | yc-review | yc-product | Product unliked |
| `review.google_reviews.refreshed.v1` | yc-review | — | Google reviews cache updated |
| `review.prompt_requested.v1` | yc-review | yc-message | Review prompt due for a delivered order (after the configured delay; consumed by yc-message) |

## Support Domain

Payloads carry IDs and the reference only; contact details and message bodies stay in yc-support (GDPR). yc-support notifies through `POST /internal/send` rather than these events until yc-message has support templates.

| Event Type | Producer | Consumers | Trigger |
|---|---|---|---|
| `support.request.created.v1` | yc-support | — | Customer or visitor opens a request |
| `support.message.posted.v1` | yc-support | — | Message added after the first (shop, customer, visitor) or the system's idle-close note |
| `support.request.closed.v1` | yc-support | — | Shop admin closes, or 72 hours pass without a message |

## Checkout Domain

| Event Type | Producer | Consumers | Trigger |
|---|---|---|---|
| `checkout.started.v1` | yc-checkout | yc-analytics, yc-monitor | Session created |
| `checkout.priced.v1` | yc-checkout | yc-analytics | PRICING complete |
| `checkout.shipping_selected.v1` | yc-checkout | — | Shipping option chosen |
| `checkout.confirmed.v1` | yc-checkout | yc-analytics | Confirm step accepted; moves to RESERVING |
| `checkout.validation_failed.v1` | yc-checkout | — | Confirm rejected (missing data) |
| `checkout.compensating.v1` | yc-checkout | — | Entering compensation |
| `checkout.completed.v1` | yc-checkout | yc-analytics, yc-cart, yc-monitor | COMPLETED |
| `checkout.failed.v1` | yc-checkout | yc-analytics, yc-monitor | Compensation finished, saga failed |
| `checkout.expired.v1` | yc-checkout | yc-analytics | Session TTL reached |

## Payment Domain

| Event Type | Producer | Consumers | Trigger |
|---|---|---|---|
| `payment.intent.created.v1` | yc-payment | — | Transaction record created, before gateway call |
| `payment.authorized.v1` | yc-payment | yc-analytics, yc-checkout | Authorization succeeded (Variant A) |
| `payment.authorization_failed.v1` | yc-payment | yc-analytics, yc-checkout, yc-monitor | Authorization declined |
| `payment.captured.v1` | yc-payment | yc-analytics, yc-checkout | Capture succeeded (Variant A) |
| `payment.capture_failed.v1` | yc-payment | yc-analytics, yc-checkout, yc-monitor | Capture declined |
| `payment.charged.v1` | yc-payment | yc-analytics, yc-checkout | Combined charge succeeded (Variant B) |
| `payment.charge_failed.v1` | yc-payment | yc-analytics, yc-checkout, yc-monitor | Combined charge declined |
| `payment.voided.v1` | yc-payment | — | Authorization voided |
| `payment.void_failed.v1` | yc-payment | — | Void failed (manual intervention) |
| `payment.refund_initiated.v1` | yc-payment | — | Refund requested |
| `payment.refund_completed.v1` | yc-payment | yc-orders | Refund settled |
| `payment.refund_failed.v1` | yc-payment | — | Refund failed |
| `payment.chargeback_received.v1` | yc-payment | yc-audit, yc-orders | Stripe dispute webhook |
| `payment.webhook_signature_failed.v1` | yc-payment | yc-audit, yc-monitor | Potential security event |

## Order Domain

| Event Type | Producer | Consumers | Trigger |
|---|---|---|---|
| `order.placed.v1` | yc-orders | yc-analytics, yc-message, yc-monitor, yc-product | Order confirmed after payment (`POST /internal/orders/{id}/confirm`) |
| `order.packaged.v1` | yc-orders | — | Shop admin packaged the order; yc-shipping booked the shipment (ADR-022) |
| `order.shipped.v1` | yc-orders | yc-message | `shipping.in_transit.v1` received |
| `order.delivered.v1` | yc-orders | yc-analytics, yc-message, yc-review | `shipping.delivered.v1` received |
| `order.cancelled.v1` | yc-orders | yc-analytics, yc-message, yc-shipping | Order cancelled |
| `order.cancellation_failed.v1` | yc-orders | — | Cancellation blocked (already shipped) (future) |
| `order.refunded.v1` | yc-orders | yc-analytics, yc-audit, yc-message | Refund recorded (cancellation, accepted return or admin refund) |
| `order.return_requested.v1` | yc-orders | — | Customer requested return |
| `order.return_accepted.v1` | yc-orders | — | Shop admin accepted return (and refunded it) |
| `order.return_rejected.v1` | yc-orders | — | Shop admin rejected return |
| `order.return_received.v1` | yc-orders | — | Returned items received (future, with return shipments) |
| `order.disputed.v1` | yc-orders | yc-audit, yc-monitor | `payment.chargeback_received.v1` received |
| `order.note_added.v1` | yc-orders | — | Note added (internal or customer-visible) |

## Shipping Domain

| Event Type | Producer | Consumers | Trigger |
|---|---|---|---|
| `shipping.rate_quoted.v1` | yc-shipping | — | Quote returned to checkout (optional telemetry; future) |
| `shipping.label_created.v1` | yc-shipping | — | Shipment booked (`POST /internal/shipments`): tracking number and label created |
| `shipping.label_failed.v1` | yc-shipping | — | Label creation failed (future, carrier drivers) |
| `shipping.carrier_pickup.v1` | yc-shipping | — | Carrier collected the package (future, carrier drivers) |
| `shipping.in_transit.v1` | yc-shipping | yc-orders | Shipment is moving |
| `shipping.out_for_delivery.v1` | yc-shipping | — | Final-mile started (future, carrier drivers) |
| `shipping.delivered.v1` | yc-shipping | yc-orders | Delivered — triggers yc-orders transition |
| `shipping.delivery_exception.v1` | yc-shipping | yc-message, yc-orders | Problem (failed delivery, address issue) |
| `shipping.cancelled.v1` | yc-shipping | — | Shipment cancelled (`order.cancelled.v1` before dispatch) |

## Message Domain

| Event Type | Producer | Consumers | Trigger |
|---|---|---|---|
| `message.queued.v1` | yc-message | — | Message persisted, ready to send |
| `message.sent.v1` | yc-message | — | Handed to provider |
| `message.delivered.v1` | yc-message | — | Provider confirmed delivery |
| `message.opened.v1` | yc-message | — | Email opened (best-effort) |
| `message.bounced.v1` | yc-message | yc-preference | Hard bounce — triggers channel unverification |
| `message.failed.v1` | yc-message | — | Exhausted retries |
| `message.verification_sent.v1` | yc-message | — | Verification code dispatched |
| `message.verification_confirmed.v1` | yc-message | yc-preference | User entered correct code |

## Image Domain

| Event Type | Producer | Consumers | Trigger |
|---|---|---|---|
| `image.uploaded.v1` | yc-image | yc-image (renders the variants) | Upload accepted, processing started |
| `image.processed.v1` | yc-image | — | All variants ready; CDN URLs populated |
| `image.processing_failed.v1` | yc-image | — | Validation or processing failed |
| `image.deleted.v1` | yc-image | yc-product | Image deleted (triggers downstream cleanup) |

## Audit Domain

| Event Type | Producer | Consumers | Trigger |
|---|---|---|---|
| `audit.record.stored.v1` | yc-audit | — | Any audit record stored (future; not published yet) |
| `audit.gdpr.cascade_complete.v1` | yc-audit | yc-audit (records it) | Every expected service reported |
| `audit.gdpr.cascade_incident.v1` | yc-audit | yc-audit, yc-monitor | Deadline (72h) passed without every report |

## Monitor Domain

| Event Type | Producer | Consumers | Trigger |
|---|---|---|---|
| `monitor.gateway.request_received.v1` | yc-gateway | — | **Not published per request** — sampled metrics only, aggregated by yc-monitor |
| `monitor.service.unhealthy.v1` | yc-monitor | — | Three failed `/readyz` checks in a row |
| `monitor.service.recovered.v1` | yc-monitor | — | Three good checks after unhealthy |
| `monitor.alert.opened.v1` | yc-monitor | yc-message, yc-monitor (critical webhook) | New alert raised |
| `monitor.alert.resolved.v1` | yc-monitor | — | Alert condition cleared, or an incident alert acknowledged |
| `monitor.alert.acknowledged.v1` | yc-monitor | — | Admin acknowledged |
| `monitor.dlq.spike_detected.v1` | yc-monitor | — | Not published: a spike opens a `dlq_rate` alert (ADR-025) |
| `monitor.threshold.breached.v1` | yc-monitor | — | Not published: breaches open alerts (ADR-025) |

## Analytics Domain

| Event Type | Producer | Consumers | Trigger |
|---|---|---|---|
| `analytics.aggregate.rebuilt.v1` | yc-analytics | — | Nightly or on-demand rebuild finished |
| `analytics.milestone.reached.v1` | yc-analytics | yc-message | Shop reaches a milestone (first order, an order count, a revenue tier) — emailed to the shop admins |

## Pattern Subscriptions

| Pattern | Subscriber | Purpose |
|---|---|---|
| `*.admin_action.v1` | yc-audit | Store as `admin_action` (future; Phase 8 audits the event types listed in ADR-025 rule 5) |
| `yc.*.*.dlq.v1` | yc-monitor | Record DLQ entry; raise alert if threshold breached |
