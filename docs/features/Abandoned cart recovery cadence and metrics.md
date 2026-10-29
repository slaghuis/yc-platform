# Abandoned cart recovery cadence and metrics

## Overview
**Status:** Approved (implemented; awaiting release tag)
**GitHub Issues:** [#35](https://github.com/slaghuis/yc-platform/issues/35)
**Priority:** High (Wisefood **JRN-014** Abandoned Basket Recovery; fit-gap **GAP-011** / **CART-005**; related CART-002 / API-Q005 TTL out of scope)
**Repository:** yc-platform — primarily **yc-cart** (idle + recovery schedule); **yc-message** (multi-touch templates / send); **yc-analytics** (abandonment & recovery rates); **yc-product** / **yc-image** (reminder imagery). Optional promo hook depends on [#31](https://github.com/slaghuis/yc-platform/issues/31). Storefront/BFF consumes OpenAPI (ADR-028).
**Work package:** WP6 (after WP5 / #33)

## Requirement

Give shops a **complete abandoned-cart recovery capability**:

1. Detect abandonment when a **customer** shopping cart with lines is idle (no product add **and** no cart-view refresh).
2. Send an **automatic multi-touch cadence** (defaults **1h / 24h / 72h**) with rich reminder content (product image(s) + resume link).
3. **Suppress** further touches when the cart changes, converts, the customer opts out, or the recovery window ends.
4. Optionally attach a **limited-time recovery discount** on a configured touch (likely final) — may wait on promotional pricing (#31).
5. Expose shop-admin **cart abandonment rate** and **cart recovery rate** with documented formulae (not only the existing funnel).

### Current behaviour (as-built)

| Surface | Today |
|---|---|
| Idle detection | Customer shopping carts idle **1h** → `cart.abandoned.v1` + recovery touch 1 |
| Activity clock | Mutations **and GET with lines** touch; cancel recovery |
| Messaging | `cart_abandoned` / `_followup` / `_final` on `cart.recovery.touched.v1` (transactional); `/cart?recover=` |
| Cadence | Touches at +0 / +24h / +72h when fingerprint unchanged |
| Admin | `GET /admin/abandoned-carts`; `GET /admin/analytics/cart-recovery` |
| Visitors | Not announced |

Citations: [yc-cart Phase 7 abandoned carts](../services/yc-cart.md); [ADR-023 messaging](../adr/ADR-023%20Customer%20Messaging.md); funnel facts in [yc-analytics](../services/yc-analytics.md).

---

## Decisions (approved 2026-10-29)

| Topic | Decision |
|---|---|
| Owning schedule | **yc-cart** owns recovery state and emits one event per touch; **yc-message** renders/sends (ADR-023). |
| Idle definition | Idle = no mutate **and** no cart **GET**. `GET /carts/{type}` **touches** when the cart has lines. |
| Cadence | **A:** idle **1h** → abandon + send #1; then **+24h** / **+72h** from `abandoned_at` if fingerprint unchanged. |
| Audience | **Customers only**. |
| Message types | `cart_abandoned`, `cart_abandoned_followup`, `cart_abandoned_final` (transactional). |
| Resume link | Storefront **`/cart`** (optional `?recover=` for attribution). |
| Optional discount | **Deferred to #31**. |
| Consent | **Transactional** (lawful-basis note for shops). |
| Metrics | Abandonment = abandoned / carts created (funnel types); recovery = converted within **7d** of `abandoned_at` / messaged (abandoned with ≥1 touch). |
| Config | Platform env/config (`YC_CART_ABANDONED_AFTER` default **1h**). |

---

## Recovery state machine

```
  active cart with lines
        │ idle ≥ idle_after (default 1h); view/mutate resets clock
        ▼
  abandoned (abandoned_at set; fingerprint stored)
        │ touch1 @ +0     → cart_abandoned
        │ touch2 @ +24h   → follow-up (if fingerprint+status unchanged)
        │ touch3 @ +72h   → final (if unchanged)
        │
        ├─ mutate / convert / clear / empty / view ──► cancel remaining; clear abandoned_at
        └─ after touch3 ──► campaign complete (stay abandoned until activity)
```

---

## Metrics formulae

| Metric | Formula |
|---|---|
| **Cart abandonment rate** | `abandoned_carts / carts_created` in `[from,to)` — abandoned = distinct carts with first abandon in period; created = distinct funnel `cart.created` in period. |
| **Cart recovery rate** | `recovered / messaged` — messaged = abandoned carts in period; recovered = those with `cart.converted` within **7 days** of `abandoned_at`. |

`GET /admin/analytics/cart-recovery?from=&to=` (+ export).

---

## Acceptance criteria

- [x] Idle rules documented: no add **and** cart view GET resets idle; default idle **1h**.
- [x] Automatic 3-touch cadence (defaults 0 / 24h / 72h from `abandoned_at`); no admin manual trigger required for the happy path.
- [x] Reminder includes ≥1 product image URL (when available) and a `/cart` resume link.
- [x] Touches 2–3 suppressed when content/status changed; convert/clear/view cancels campaign.
- [x] Optional discount deferred to #31.
- [x] Shop-admin abandonment rate + recovery rate APIs with documented formulae.
- [x] OpenAPI + MCP; docs updated.
- [x] GDPR: recovery state deleted with carts on `identity.user.deleted.v1`.

---

## Out of scope (MVP)

- Push / on-site popups beyond existing channel drivers.
- Visitor cart recovery by email capture.
- Visitor cart TTL (API-Q005).
- Full promo engine (#31).
- Per-shop cadence UI (config/env first).

---

## Review checklist (approved 2026-10-29)

- [x] Cadence mapping **A** (idle 1h → abandon + send #1; +24h / +72h).
- [x] Consent: **transactional**.
- [x] Resume link `/cart`.
- [x] Discount deferred to #31.
- [x] Metric formulae + **7d** recovery window; denominator = carts created.
- [x] Customers-only audience.
