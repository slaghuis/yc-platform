# Service Requirement Specifications

One file per service, matching `/services/<name>/`, plus the shared event library and schema repository.

| Service | Database | Spec |
|---|---|---|
| yc-identity    | yc_identity    | [yc-identity.md](yc-identity.md) |
| yc-gateway     | — (stateless)  | [yc-gateway.md](yc-gateway.md) |
| yc-image       | yc_image       | [yc-image.md](yc-image.md) |
| yc-preference  | yc_preference  | [yc-preference.md](yc-preference.md) |
| yc-product     | yc_product     | [yc-product.md](yc-product.md) |
| yc-pricing     | yc_pricing     | [yc-pricing.md](yc-pricing.md) |
| yc-inventory   | yc_inventory   | [yc-inventory.md](yc-inventory.md) |
| yc-cart        | yc_cart        | [yc-cart.md](yc-cart.md) |
| yc-review      | yc_review      | [yc-review.md](yc-review.md) |
| yc-checkout    | yc_checkout    | [yc-checkout.md](yc-checkout.md) |
| yc-payment     | yc_payment     | [yc-payment.md](yc-payment.md) |
| yc-shipping    | yc_shipping    | [yc-shipping.md](yc-shipping.md) |
| yc-orders      | yc_orders      | [yc-orders.md](yc-orders.md) |
| yc-message     | yc_message     | [yc-message.md](yc-message.md) |
| yc-audit       | yc_audit       | [yc-audit.md](yc-audit.md) |
| yc-monitor     | yc_monitor     | [yc-monitor.md](yc-monitor.md) |
| yc-analytics   | yc_analytics   | [yc-analytics.md](yc-analytics.md) |
| yc-tenant      | yc_tenant      | [yc-tenant.md](yc-tenant.md) |

Shared components:

| Component | Spec |
|---|---|
| `libs/yc-eventbus` | [yc-eventbus.md](yc-eventbus.md) |
| `schemas/yc-event-schemas` | [yc-event-schemas.md](yc-event-schemas.md) |
