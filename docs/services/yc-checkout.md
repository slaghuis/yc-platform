# yc-checkout — Service Requirement

## Overview

`yc-checkout` orchestrates the checkout saga. It takes a cart snapshot, drives it through pricing, inventory reservation, payment authorization, order creation, payment capture and order confirmation — with compensating transactions on any failure before payment succeeds. It calls every participant synchronously (ADR-019). It is the authoritative owner of the saga state and the entry point for the frontend's checkout UX.

## Location in Monorepo

```
/services/yc-checkout/
```

## Responsibilities

1. Start a checkout session from a cart
2. Snapshot cart items into the session
3. Call yc-pricing for authoritative pricing, including tax
4. Call yc-shipping for shipping quotes at the shipping step (when `requires_shipping`, ADR-022)
5. Validate guest contact at confirm, and that physical items have the address shipping was quoted for (ADR-010, ADR-022)
6. Reserve inventory (only tracked items; ADR-012)
7. Authorize payment via yc-payment (variant A or B per ADR-013)
8. Create a `pending_payment` order via yc-orders
9. Capture payment (variant A) or charge (variant B)
10. Confirm the order once payment has succeeded
11. Honor the quoted price for the session TTL (ADR-011)
12. Drive compensation on any step failure before payment succeeds
13. Resume from persisted state after a crash
14. Expire sessions on TTL and compensate

## Multi-Tenancy

- Sessions are tenant-scoped
- All sub-service calls carry the tenant in `X-YC-Tenant-ID`, propagated through yc-gateway

## Session Lifecycle

The full flow, calls and compensation are in [`docs/diagrams/checkout-saga.md`](../diagrams/checkout-saga.md); the rules are in ADR-019. States:

```
Variant A: STARTED → PRICING → RESERVING → AUTHORIZING → ORDER_CREATE → CAPTURING → CONFIRMING → COMPLETED
Variant B: STARTED → PRICING → RESERVING → ORDER_CREATE → CHARGING → CONFIRMING → COMPLETED

(failure before capture/charge succeeds) → COMPENSATING → FAILED
(session TTL reached before capture/charge) → COMPENSATING → EXPIRED
```

Capture or charge success is the point of no return: CONFIRMING is retried until it succeeds and is never compensated.

## Session TTL (ADR-011)

- Default: 30 minutes
- Tenant override: min 10, max 60
- At TTL expiry, YC-checkout:
  - Transitions session to `EXPIRED`
  - Runs compensation for whatever has been done
  - Emits `checkout.expired.v1`

## API

### Customer / Visitor Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| POST | `/checkout` | Start a checkout from a cart | any (customer or visitor) |
| GET | `/checkout/{session_id}` | Get session state (including current prices, shipping options) | session owner |
| POST | `/checkout/{session_id}/shipping` | Give the delivery address (quoted by yc-shipping) and/or choose a shipping option | session owner |
| POST | `/checkout/{session_id}/confirm` | Confirm checkout; runs the saga inline (Variant A: reserve → authorize → create → capture → confirm; Variant B: reserve → create → charge → confirm) | session owner |
| POST | `/checkout/{session_id}/cancel` | Cancel in-progress session | session owner |

### Internal Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| GET | `/internal/sessions/{id}` | Fetch session (used by yc-orders if needed) | service |

### Admin Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| GET | `/admin/sessions` | List sessions (filterable by state, time) | shop_admin |
| GET | `/admin/sessions/{id}` | Session detail (incl. compensation log) | shop_admin |
| POST | `/admin/sessions/{id}/force-compensate` | Manual compensation trigger (if stuck) | system_admin |

### Phase 4 to 6 Implementation Notes

The contract is `services/yc-checkout/api/openapi.yaml`. Implemented: the customer and visitor endpoints, the admin list and detail, the internal read, both saga variants with compensation, resumption, the TTLs, guest checkout, gateway routing and the `identity.user.deleted.v1` subscription (ADR-025). Not yet: `tenant_checkout_config` and per-shop TTLs, `force-compensate`, and the `platform.tenant.*` subscriptions.

- **Start** reads the cart (`GET /internal/carts/{user_id}/{type}`), the SKUs (`GET /internal/skus/batch` on yc-product) and the prices (`POST /internal/prices/evaluate`), all in one request, and routes the payment in the store currency (`GET /internal/gateway-route` on yc-payment). It stores the quote: the session is created in `PRICING` (there is no `STARTED` row) and publishes `checkout.started.v1` and `checkout.priced.v1`. An empty or non-convertible cart, unavailable SKUs, unpriced lines or a shop that takes no payments in the store currency are `409`.
- **Routing** (ADR-013). The session records the gateway, its flow (`auth_capture` for Variant A, `charge` for Variant B, from `supports_separate_auth_capture`), whether payment is hosted, and the gateway's public key, shown as `payment` on the session. Confirm may name another `payment_gateway`; it is routed again and must take the store currency (`400` otherwise). A hosted payment (Yoco) takes no `payment_gateway_token` and needs a `return_url`.
- **Variant B** creates the order before charging: `RESERVING → ORDER_CREATE → CHARGING → CONFIRMING`. The charge carries the order ID. A hosted charge answers `202` in `CHARGING` with `next_action` `redirect_to_url`; `payment.charged.v1` or `payment.charge_failed.v1` (from the gateway's webhook) resumes the session, as do confirming again and the pending poll. A declined charge compensates with `payment_declined`.
- **Late charges** (ADR-019 rule 12). Compensating a hosted charge leaves it open (`payment_voided: left_open`). If `payment.charged.v1` then arrives for a session that has already undone its payment, checkout refunds it in full (key `{session_id}:charge:refund`) and records `late_charge_refunded` in the compensation log; a session still about to void is driven instead, and goes forward when the void finds the charge captured.
- **Shipping** (Phase 6, ADR-022 rules 1–2). A session with physical items starts with no options. `POST /checkout/{id}/shipping` with a `delivery_address` calls yc-shipping `POST /internal/quote` (the lines with their SKU `weight_grams` from yc-product, and the items total) and stores the address and options, selecting `shipping_option_id` or the cheapest; with only `shipping_option_id` it chooses among the stored options. An address the shop does not ship to is `409` and changes nothing; a digital-only session is `400`. Shipping is untaxed and added to the total. Confirm needs the shipping step first, and a different `delivery_address` at confirm is `409` ("choose shipping again"). The order gets the address and the option (`id`, `method`, `rate_id`, `name`, `amount_minor`), which yc-orders books the shipment with. A digital-only session needs no address.
- **Confirm idempotency is native**, not the shared middleware: the first `Idempotency-Key` is stored on the session (`confirm_key`). The same key returns the session's current state and drives it on if it is waiting (after 3-D Secure, or a deadline), so it is safe to repeat; a different key is `409`, or `410` once the session has expired. Confirm runs the saga inline with a 60-second deadline on a context detached from the request, so a client disconnect never triggers compensation; then `200` `COMPLETED`, `422` `FAILED`/`EXPIRED`, or `202`.
- **One replica drives a session** through a row lease (`lease_owner`, `lease_until`, renewed on every save; `saga.lease_ttl`, default 2 minutes). Lease owners start with the host name, so a restarted replica frees its old leases at once (in compose, `hostname: yc-checkout`); otherwise they lapse after the TTL.
- **The sweeper** (every 30 s, and once at startup) expires sessions past `expires_at` and resumes driven sessions whose `next_attempt_at` is due and whose lease is free. A step that times out is left for the sweeper; a step that is rejected (4xx) or still failing after `calls.max_attempts` compensates. `CAPTURING` retries an unknown outcome and compensates only an outright refusal (`capture_failed`). `CONFIRMING` is retried with backoff (5 s doubling to 5 min) forever, and after `saga.alert_after` attempts logs an error with `alert=critical`.
- **Compensation runs void → abandon → release**, not the order in the matrix below: the authorisation (or pending charge) is voided first so the customer's funds are not held while the rest is undone. The void names the transaction, or the authorize or charge key when the ID was never learnt; if yc-payment answers that the payment was captured after all, the session goes forward to `CONFIRMING` instead (`capture_discovered`). An order or reservation whose ID was never learnt is found by repeating the create or reserve call with the same key. Each undo is recorded in `compensation_log`.
- **Pending payments** (3-D Secure, a hosted payment page) leave the session in `AUTHORIZING` or `CHARGING` with the payment's `next_action`; the payment events settle it, as does confirming again or the sweeper (every `saga.pending_poll`, 30 s). `POST /checkout/{id}/cancel` works in `PRICING` and while a payment is pending; otherwise it is `409`.
- **Customers see only** `shortages` (out of stock) and `decline_reason` in a failure's detail; shop admins see all of it, plus the history and compensation log.
- **Dev pause.** `debug.pause_before` (a state) and `debug.pause_for` (`YC_CHECKOUT_PAUSE_BEFORE`, `YC_CHECKOUT_PAUSE_FOR`) stall the saga before a step, for the kill-and-resume demo. The service refuses to start with them in production.
- **yc-cart** removes the checked-out quantities on `checkout.completed.v1`, rather than being called to clear the cart.

## Confirm Request

```json
POST /checkout/{session_id}/confirm
{
  "guest_contact": {              // required if actor is visitor
    "email": "...",
    "phone_e164": "+27831234567"
  },
  "delivery_address": { ... },    // optional: must be the address given at the shipping step (409 otherwise)
  "shipping_option_id": "...",    // optional: one of the session's options; defaults to the selected one
  "payment_gateway": "stripe",    // optional: another enabled gateway than the session's
  "payment_gateway_token": "...", // token from the gateway SDK (ADR-009); not for a hosted gateway
  "return_url": "https://..."     // where the gateway sends the customer back (3-D Secure, hosted page)
}
```

Requires `Idempotency-Key` (`api-conventions.md` §10). Responses (ADR-019):

| Status | Session state | Meaning |
|---|---|---|
| `200` | `COMPLETED` | Paid; body includes `order_id` |
| `202` | in progress | Payment needs customer action (body includes `next_action`), compensation is still retrying, or the 60 s request deadline passed. Poll `GET /checkout/{session_id}`; after completing `next_action`, call confirm again with the same `Idempotency-Key` |
| `422` | `FAILED` | Rejected (out of stock, card declined, …) and compensation finished; body includes the reason |

## Database Schema

Database: **`yc_checkout`**

```sql
CREATE TABLE tenant_checkout_config (
    tenant_id              TEXT PRIMARY KEY,
    session_ttl_minutes    INT NOT NULL DEFAULT 30,
    reservation_ttl_minutes INT NOT NULL DEFAULT 15,   -- gateway routing lives in yc-payment (ADR-013)
    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE checkout_sessions (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               TEXT NOT NULL,
    user_id                 TEXT NOT NULL,
    user_type               TEXT NOT NULL,          -- 'visitor','customer'
    cart_id                 UUID NOT NULL,
    state                   TEXT NOT NULL,          -- see state machine
    requires_shipping       BOOLEAN NOT NULL,
    has_tracked_items       BOOLEAN NOT NULL,
    items_snapshot          JSONB NOT NULL,         -- from cart at START
    pricing_snapshot        JSONB,                   -- from yc-pricing at PRICING
    shipping_options_snapshot JSONB,                 -- from yc-shipping at the shipping step
    selected_shipping_option_id TEXT,
    guest_contact           JSONB,                   -- email, phone
    delivery_address        JSONB,
    payment_gateway         TEXT,                    -- routed at start (ADR-013)
    payment_variant         TEXT NOT NULL DEFAULT 'auth_capture', -- 'auth_capture' (A), 'charge' (B)
    hosted_payment          BOOLEAN NOT NULL DEFAULT false,
    payment_public_key      TEXT,
    payment_gateway_token   TEXT,                    -- transient; cleared post-use
    reservation_id          UUID,
    reservation_expires_at  TIMESTAMPTZ,
    payment_intent_id       TEXT,
    payment_auth_id         TEXT,
    order_id                UUID,
    compensation_log        JSONB NOT NULL DEFAULT '[]',
    failure_reason          TEXT,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at              TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_sessions_tenant_user ON checkout_sessions (tenant_id, user_id);
CREATE INDEX idx_sessions_resumable ON checkout_sessions (state, expires_at)
    WHERE state NOT IN ('COMPLETED','FAILED','EXPIRED');

CREATE TABLE session_events (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id      UUID NOT NULL REFERENCES checkout_sessions(id) ON DELETE CASCADE,
    event_type      TEXT NOT NULL,
    from_state      TEXT,
    to_state        TEXT,
    payload         JSONB,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_session_events ON session_events (session_id, created_at);

CREATE TABLE outbox ( ... );
CREATE TABLE processed_events ( ... );

ALTER TABLE checkout_sessions ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON checkout_sessions
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform');
```

## Resumption on Startup

On start, yc-checkout:
1. Queries `checkout_sessions WHERE state NOT IN ('COMPLETED','FAILED','EXPIRED')`
2. For each session:
   - If `expires_at < NOW()`: transition to EXPIRED and run compensation
   - Else: continue the saga from the persisted state
   - Sessions in `CONFIRMING` are always driven forward, whatever their `expires_at`
3. All transitions are idempotent — the in-flight call is repeated with the same `Idempotency-Key: {session_id}:{step}`, and the participant returns its stored result

## Published Events

| Event | When |
|---|---|
| `checkout.started.v1` | Session created |
| `checkout.priced.v1` | PRICING complete |
| `checkout.shipping_selected.v1` | Delivery address quoted and/or shipping option chosen |
| `checkout.confirmed.v1` | Confirm step accepted; moves to RESERVING |
| `checkout.validation_failed.v1` | Confirm rejected (missing data) |
| `checkout.compensating.v1` | Entering compensation |
| `checkout.completed.v1` | COMPLETED |
| `checkout.failed.v1` | Compensation finished, saga failed |
| `checkout.expired.v1` | Session TTL reached |

No command events: participants are called synchronously (ADR-019).

## Synchronous Calls

| Step | Call | Compensation |
|---|---|---|
| START | yc-cart `GET /internal/carts/{user_id}/{type}` | — |
| PRICING | yc-pricing `POST /internal/evaluate`; yc-shipping `POST /internal/quote` at the shipping step | — |
| RESERVING | yc-inventory `POST /internal/reservations` | `POST /internal/reservations/{id}/release` |
| AUTHORIZING (A) | yc-payment `POST /internal/authorize` | `POST /internal/void` |
| ORDER_CREATE | yc-orders `POST /internal/orders` | `POST /internal/orders/{id}/abandon` |
| CAPTURING (A) | yc-payment `POST /internal/capture` | — (point of no return) |
| CHARGING (B) | yc-payment `POST /internal/charge` | `POST /internal/void` while pending; a late charge is refunded (`POST /internal/refund`) |
| CONFIRMING | yc-orders `POST /internal/orders/{id}/confirm`, yc-inventory `POST /internal/reservations/{id}/commit` | — (forward only) |

Mutating calls carry `Idempotency-Key: {session_id}:{step}`; compensation calls append `:undo`.

## Subscribed Events

| Event | Action |
|---|---|
| `inventory.stock.reservation_expired.v1` | If the session is active and before the point of no return: compensate from the current state |
| `payment.authorized.v1` / `payment.authorization_failed.v1` | Resume a session waiting on a `pending` authorization (3-D Secure, delayed confirmation) |
| `payment.charged.v1` / `payment.charge_failed.v1` | Resume a session waiting on a `pending` charge; refund a charge that arrives after the session ended (ADR-019 rule 12) |
| `payment.captured.v1` / `payment.capture_failed.v1` | Resume a session waiting on a `pending` capture |
| `identity.user.deleted.v1` | Pseudonymize `user_id` on non-terminal sessions; delete `guest_contact` on terminal sessions beyond retention window |
| `platform.tenant.provisioned.v1` | Create `tenant_checkout_config` |
| `platform.tenant.deleted.v1` | Delete tenant sessions beyond legal retention |

## Compensation Matrix (recap)

| Failed Step | Must Undo |
|---|---|
| PRICING | Nothing |
| RESERVING | Nothing |
| AUTHORIZING (A) | Release reservation (if any) |
| ORDER_CREATE | Void auth (Variant A), release reservation (if any) |
| CAPTURING (A) | Abandon order, void auth, release reservation (if any) |
| CHARGING (B) | Void the pending charge (a hosted one is left open, and refunded if paid later), abandon order, release reservation (if any) |
| CONFIRMING | Never compensated — retried until yc-orders confirms |

Compensation is idempotent and ordered. Compensation calls use `Idempotency-Key: {session_id}:{step}:undo` and treat "nothing to undo" as success.

## Gateway Routing

When the session starts (ADR-013):
1. yc-checkout asks yc-payment `GET /internal/gateway-route?currency={store currency}`. yc-payment owns the shop's gateway configs: among the enabled gateways whose driver takes the currency and whose `currencies` list (if any) includes it, the lowest `priority` wins.
2. The chosen gateway's capability (`supports_separate_auth_capture`) determines Variant A or B; `hosted_payment` decides whether confirm needs a token or a return URL
3. yc-checkout stores the gateway, flow and capabilities on the session. A `payment_gateway` named at confirm is routed again with `&gateway=`.

## Configuration

```yaml
yc-checkout:
  listen: :8080
  database:
    url: postgres://yc_checkout:...@...
  defaults:
    session_ttl_minutes: 30
    reservation_ttl_minutes: 15
  resume_on_startup: true
  expiry_sweeper_interval_seconds: 30
  calls:
    timeout_seconds: 10
    payment_timeout_seconds: 35     # above the gateway timeout (30 s)
    max_attempts: 3
    confirm_deadline_seconds: 60    # then respond 202 and continue in the background
    confirming_alert_minutes: 5
  nats:
    url: nats://...
  log_level: info
```

## Dependencies on Other Services

- **yc-cart** (session start — read & snapshot)
- **yc-pricing** (authoritative pricing)
- **yc-shipping** (shipping quotes)
- **yc-inventory** (reservations)
- **yc-payment** (authorization, capture)
- **yc-orders** (order creation and confirmation)
- **yc-preference** (display currency, locale — optional)

All inter-service calls are synchronous `/internal/` calls through yc-gateway with yc-checkout's service token, propagating the tenant, actor and correlation headers (ADR-019, `api-conventions.md` §9).

## Called By

- Frontend clients
- Shop admins (read-only session inspection)

## GDPR Behavior

- `guest_contact` and `delivery_address` are PII
- On `identity.user.deleted.v1`:
  - Non-terminal sessions: pseudonymize `user_id`
  - Terminal sessions (COMPLETED/FAILED/EXPIRED): retain for operational audit window (90 days), then purge PII
- `payment_gateway_token` is cleared immediately after successful authorization/charge (never retained beyond use)
- Publish `platform.gdpr.cascade_completed.v1` when done

*Phase 8:* Implemented: every session of the user, terminal or not, gets the pseudonym and loses its guest contact and delivery address at once; the action is reported as `pseudonymized`.

## Open Questions / Future Scope

- Multiple shipment splitting (one order, multiple deliveries)
- Promo code application at confirm (currently assumed to be part of cart)
- Checkout-level discounts applied by shop admin manually (concessions)
- Alternative checkouts (one-click, Apple Pay, Google Pay) — gateway-specific flows
- Subscription checkouts (recurring billing)
- Deferred payment (invoice, net-30 for B2B)
- Order splitting across suppliers
- Partial fulfillment (some items available, some backordered — not in v1)
