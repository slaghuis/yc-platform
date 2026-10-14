### `docs/services/README.md`

```markdown
# Service Requirement Specifications

One file per service, matching `/services/<name>/`.

| Service | Database | Spec |
|---|---|---|
| yc-identity    | yc_identity    | yc-identity.md |
| yc-gateway     | — (stateless)  | yc-gateway.md |
| yc-image       | yc_image       | yc-image.md |
| yc-preference  | yc_preference  | yc-preference.md |
| yc-product     | yc_product     | yc-product.md |
| yc-pricing     | yc_pricing     | yc-pricing.md |
| yc-inventory   | yc_inventory   | yc-inventory.md |
| yc-cart        | yc_cart        | yc-cart.md |
| yc-review      | yc_review      | yc-review.md |
| yc-checkout    | yc_checkout    | yc-checkout.md |
| yc-payment     | yc_payment     | yc-payment.md |
| yc-shipping    | yc_shipping    | yc-shipping.md |
| yc-orders      | yc_orders      | yc-orders.md |
| yc-message     | yc_message     | yc-message.md |
| yc-audit       | yc_audit       | yc-audit.md |
| yc-monitor     | yc_monitor     | yc-monitor.md |
| yc-analytics   | yc_analytics   | yc-analytics.md |

Each spec was produced in earlier design sessions; populate each file from
those sessions.