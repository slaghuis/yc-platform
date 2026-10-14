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
4. Call yc-shipping for shipping quotes (when `requires_shipping`)
5. Validate guest contact and delivery address at confirm (ADR-010)
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
| POST | `/checkout/{session_id}/shipping` | Select a shipping option | session owner |
| POST | `/checkout/{session_id}/confirm` | Confirm checkout; runs reserve → authorize → create → capture → confirm inline | session owner |
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

## Confirm Request

```json
POST /checkout/{session_id}/confirm
{
  "guest_contact": {              // required if actor is visitor
    "email": "...",
    "phone_e164": "+27831234567"
  },
  "delivery_address": { ... },    // required if requires_shipping = true (ADR-015: free-form JSON)
  "shipping_option_id": "...",    // required if requires_shipping
  "payment_gateway": "yoco",      // which gateway to use
  "payment_gateway_token": "..."  // token from the gateway SDK (ADR-009)
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
    reservation_ttl_minutes INT NOT NULL DEFAULT 15,
    enabled_gateways       TEXT[] NOT NULL,
    gateway_routing        JSONB NOT NULL DEFAULT '{}',  -- currency → gateway priority
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
    shipping_options_snapshot JSONB,                 -- from yc-shipping at PRICING
    selected_shipping_option_id TEXT,
    guest_contact           JSONB,                   -- email, phone
    delivery_address        JSONB,
    payment_gateway         TEXT,
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
| `checkout.shipping_selected.v1` | Shipping option chosen |
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
| PRICING | yc-pricing `POST /internal/evaluate`, yc-shipping `POST /internal/quote` | — |
| RESERVING | yc-inventory `POST /internal/reservations` | `POST /internal/reservations/{id}/release` |
| AUTHORIZING (A) | yc-payment `POST /internal/authorize` | `POST /internal/void` |
| ORDER_CREATE | yc-orders `POST /internal/orders` | `POST /internal/orders/{id}/abandon` |
| CAPTURING (A) | yc-payment `POST /internal/capture` | — (point of no return) |
| CHARGING (B) | yc-payment `POST /internal/charge` | — (point of no return) |
| CONFIRMING | yc-orders `POST /internal/orders/{id}/confirm`, yc-inventory `POST /internal/reservations/{id}/commit` | — (forward only) |

Mutating calls carry `Idempotency-Key: {session_id}:{step}`; compensation calls append `:undo`.

## Subscribed Events

| Event | Action |
|---|---|
| `inventory.stock.reservation_expired.v1` | If the session is active and before the point of no return: compensate from the current state |
| `payment.authorized.v1` / `payment.authorization_failed.v1` | Resume a session waiting on a `pending` authorization (3-D Secure, delayed confirmation) |
| `payment.charged.v1` / `payment.charge_failed.v1` | Resume a session waiting on a `pending` charge |
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
| CHARGING (B) | Abandon order, release reservation (if any) |
| CONFIRMING | Never compensated — retried until yc-orders confirms |

Compensation is idempotent and ordered. Compensation calls use `Idempotency-Key: {session_id}:{step}:undo` and treat "nothing to undo" as success.

## Gateway Routing

At the start of AUTHORIZING / CHARGING:
1. yc-checkout determines the gateway based on:
   - Tenant's `enabled_gateways`
   - `gateway_routing` map (store currency → gateway priority)
   - Fallback to first enabled gateway
2. The chosen gateway's capability (`supports_separate_auth_capture`) determines Variant A or B
3. yc-checkout stores the chosen gateway on the session

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

## Open Questions / Future Scope

- Multiple shipment splitting (one order, multiple deliveries)
- Promo code application at confirm (currently assumed to be part of cart)
- Checkout-level discounts applied by shop admin manually (concessions)
- Alternative checkouts (one-click, Apple Pay, Google Pay) — gateway-specific flows
- Subscription checkouts (recurring billing)
- Deferred payment (invoice, net-30 for B2B)
- Order splitting across suppliers
- Partial fulfillment (some items available, some backordered — not in v1)
