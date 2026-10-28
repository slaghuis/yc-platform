# Guest order access links

## Overview
**Status:** Implemented
**GitHub Issues:** [#22](https://github.com/slaghuis/yc-platform/issues/22)
**Priority:** High (guest receipt from email on another device)
**Repository:** yc-platform — yc-orders + yc-message; storefront URL contract (ADR-027 rule 12).

## Requirement

A guest who opens the order confirmation (or later order) email on another device must be able to view that order without the original `yc_visitor` cookie.

**Decision (product):** yes — use a **signed, time-limited** link. Update the order API and the storefront URL contract together.

## Decisions

| Topic | Choice |
|---|---|
| Mechanism | Stateless HMAC-SHA256 token (no DB row); secret + TTL on yc-orders |
| Storefront URL | `/orders/{order_id}?guest_token=…` (path unchanged; query added for guests) |
| Order API | `GET /orders/{id}?guest_token=` accepts a valid token for a **placed visitor** order |
| Who mints | yc-orders `GET /internal/orders/{id}/guest-access` → `{token, expires_at}`; yc-message calls it when building guest `order_url` |
| Customer orders | Unchanged plain `/orders/{id}` (session / login) |
| Mutations | Token is **read-only** (GET). Cancel/return still require the owning visitor or customer |
| TTL | Default **30 days** (`guest_access.ttl`), configurable |
| Failure mode | Invalid / expired / wrong-order token → same **404** as a non-owner |
| Events | Token never appears on the bus (same stance as password-reset tokens) |

## Acceptance criteria

- [x] Another visitor (or a fresh visitor cookie) can `GET /orders/{id}` with a valid `guest_token` and receive the guest order.
- [x] Without a matching visitor id and without a valid token → 404.
- [x] Tampered or expired token → 404.
- [x] Token does not authorise GET of a customer order or an unplaced order.
- [x] Guest order emails from yc-message include `?guest_token=` on `order_url`.
- [x] Customer order emails keep `/orders/{id}` without a token.
- [x] ADR-027 rule 12 table, `api-conventions` §21, and `links_test.go` document `/orders/{order_id}?guest_token=…` for guest mail.

## Out of scope

- Single-use / revocable tokens.
- Granting mutate APIs via the token.
- Associating guest orders with a later registration (existing future work in yc-orders.md).
