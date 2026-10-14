# ADR-009: No Storage of Payment Card Data
**Status**: Accepted
**Date**: 2025-01-15 
**Context**: Payment processing requires handling sensitive card data. Storing this data brings significant PCI-DSS compliance obligations (SAQ D, annual audits, infrastructure controls, insurance). We need to decide our PCI scope.
## Decision
 - yc-payment never stores, processes, or transmits raw card data.
 - All card entry is handled by the external payment gateway's hosted fields, iframes, or redirect flow. yc-payment only ever handles gateway-issued tokens (payment method tokens, charge IDs, transaction IDs).
## Rules
 - The frontend talks directly to the payment gateway's SDK for card capture (Stripe Elements, Yoco SDK, etc.) — the yc backend is never in the card data path.
 - The frontend receives a gateway token from the gateway SDK.
 - The frontend passes this token to yc-checkout, which passes it to yc-payment.
 - yc-payment uses the token to authorize/capture via the gateway's API.
 - No logging of tokens that could be reused — treat them as sensitive.
 - No saved payment methods in v1 — every checkout requires fresh card entry via the gateway. (Future: gateway-side saved methods, where we store only the gateway's customer_id reference, not card data.)
 - Our PCI scope target: SAQ A (merchants who outsource all card data handling to a PCI-DSS validated third party).
## Consequences
### Positive
 - Minimum PCI scope (SAQ A) — simplest annual compliance.
 - No card data on our infrastructure means no card data breach is possible for us.
 - No encryption key management for card data.
 - Audits are far cheaper and faster.
### Negative
 - Users must re-enter card data for every purchase in v1 (acceptable tradeoff).
 - Tightly coupled to gateway SDKs on the frontend.
 - Switching gateways requires frontend changes, not just backend.
## Future Evolution
 - When saved payment methods are needed, we will store only the gateway's customer_id and payment_method_id references in yc-payment. Still SAQ A, since the gateway holds the card data. This remains out of scope for v1.
