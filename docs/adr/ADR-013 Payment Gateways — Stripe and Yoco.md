# ADR-013: Payment Gateways — Stripe and Yoco

**Status:** Accepted
**Date:** 2025-01-15
**Context:** The primary market is South Africa. Local payment acceptance (ZAR, local cards, SnapScan-style flows, EFT) is essential. International acceptance remains important.

## Decision

YC-Payment ships v1 with **two gateway drivers**:

1. **Yoco** — primary gateway for South African merchants. Supports ZAR, local card acquiring, and is widely trusted locally.
2. **Stripe** — secondary gateway for international acceptance and tenants targeting cross-border customers.

YC-Payment is designed as a **gateway-agnostic abstraction**: each gateway is a driver implementing a common interface. Adding a third gateway later (PayGate, Peach Payments, PayPal) is a driver, not a redesign.

## Gateway Driver Interface

Each driver implements:

```go
type GatewayDriver interface {
    Name() string
    Authorize(ctx, AuthorizeRequest) (AuthorizeResponse, error)
    Capture(ctx, CaptureRequest) (CaptureResponse, error)
    Void(ctx, VoidRequest) (VoidResponse, error)
    Refund(ctx, RefundRequest) (RefundResponse, error)
    HandleWebhook(ctx, WebhookRequest) (WebhookEvent, error)
    SupportedCurrencies() []string
    SupportsSeparateAuthCapture() bool
}
```

## Tenant Configuration

- Each tenant configures **which gateways are enabled** and in what priority order
- A tenant may enable multiple gateways and choose per-currency routing (e.g., Yoco for ZAR, Stripe for USD/EUR)
- Gateway credentials (API keys, webhook secrets) are stored encrypted per tenant in YC-Payment's database
- The frontend queries YC-Checkout for which gateway to use based on the cart currency and tenant config, and loads the matching gateway SDK

**Routing** (amended 2026-10-15):
- Each gateway config has a `priority` (0–1000, lowest first, default 100) and an optional `currencies` list. An empty list routes every currency the driver supports. A list naming a currency the driver does not support is rejected.
- yc-payment answers `GET /internal/gateway-route?currency=…[&gateway=…]`: among the shop's enabled gateways whose driver supports the currency and whose list (if any) includes it, the lowest priority wins, then the gateway name. With `gateway`, that gateway must qualify. No gateway is `412`. The answer is the gateway's capabilities: `supports_separate_auth_capture`, `hosted_payment` and the public key.
- yc-checkout routes when the session starts, in the store currency, and records the gateway and its saga variant on the session (`payment.gateway`, `payment.flow`, `payment.hosted`). The customer may name another of the shop's gateways at confirm; it is routed again and must qualify for the currency.

## Gateway-Specific Notes

### Yoco
- Primary flow: **Yoco Online Checkout** or **Yoco Payments API**
- Webhook-driven payment confirmation
- Note: Yoco may not support separate authorize + capture for all card types. If a gateway reports `SupportsSeparateAuthCapture() = false`, the saga uses a **combined auth+capture** step (CHARGING) and skips a distinct AUTHORIZING step.

**Hosted flow** (amended 2026-10-15). The driver uses Yoco's Checkout API (`POST /api/checkouts`, ZAR only). The customer pays on Yoco's page, so:
- There is no payment token. CHARGING creates the Yoco checkout and returns `pending` with `next_action` `{type: redirect_to_url, redirect_to_url: {url, return_url}}`; confirm answers `202` and the storefront redirects.
- The outcome arrives **only by webhook** (`payment.succeeded`, `payment.failed`, `refund.succeeded`, `refund.failed`): Yoco has no checkout lookup. Repeating the charge call returns the transaction's recorded state. Webhooks are verified with the Standard Webhooks scheme (`webhook-id`, `webhook-timestamp`, `webhook-signature`, HMAC-SHA256 with the `whsec_` secret Yoco returns when the webhook is registered, 3-minute tolerance) and de-duplicated by event ID.
- A Yoco checkout **cannot be cancelled**. Voiding a pending charge leaves it open (`void` answers `pending`). If the customer pays after the session ended, checkout refunds the charge (ADR-019 rule 12).
- A later success overrides a failure: the customer may try another card on Yoco's page.
- The capabilities are `SupportsSeparateAuthCapture() = false`, `SupportedCurrencies() = [ZAR]` and `HostedPayment() = true`. Yoco's webhook is `POST /webhooks/yoco` on the shop's host, a gateway callback route.

### Stripe
- Primary flow: **Stripe Payment Intents API** with Elements/Checkout on the frontend
- Supports separate authorize and capture
- Supports a wide range of currencies

## Saga Impact: Combined Auth+Capture Variant

Because not all gateways support separate authorize and capture, YC-Checkout supports two variants:

**Variant A (separate):** AUTHORIZING → ORDER_CREATE → CAPTURING
**Variant B (combined):** ORDER_CREATE → CHARGING (auth+capture in one step)

The variant is chosen from the routed gateway's capability when the session starts (see Routing). yc-checkout calls yc-payment synchronously (ADR-019):
- Variant A calls `POST /internal/authorize`, then `POST /internal/capture`; yc-payment publishes `payment.authorized.v1` and `payment.captured.v1` as facts
- Variant B calls `POST /internal/charge`; yc-payment publishes `payment.charged.v1`

**Compensation implication for Variant B**: no auth to void. Under ADR-019 the order is created as `pending_payment` before the charge, so a failed order creation happens before any money moves and needs no refund. Once the charge succeeds, the saga only moves forward.

*Amended 2026-10-14 by ADR-019: the saga uses synchronous internal calls instead of command events, and Variant B no longer refunds to compensate a failed order creation.*

*Amended 2026-10-15 (Phase 5): per-currency routing with priorities, the session-start route, Yoco's hosted flow, and the late-charge refund (ADR-019 rule 12) — the one case where Variant B refunds.*

**Recommendation**: prefer Variant A gateways when possible. Document per-gateway behavior in the YC-Payment service requirements.

## Consequences

**Positive**
- Local-market-first for South African tenants
- International acceptance via Stripe
- Clean extensibility to additional gateways

**Negative**
- Two gateway integrations to maintain in v1 (vs one)
- Saga must support both auth+capture variants
- Webhook infrastructure for two gateways (different signing, different formats)

## Future Gateways (not v1)

- PayGate, Peach Payments (South African alternatives)
- PayPal (international alternative)
- Direct EFT (Ozow, PayFast) — different flow entirely, likely a new saga variant
