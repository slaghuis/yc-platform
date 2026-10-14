# yc-checkout — Service Requirement

## Overview

`yc-checkout` orchestrates the checkout saga. It takes a cart snapshot, drives it through pricing, inventory reservation, payment authorization, order creation, and payment capture — with compensating transactions on any failure. It is the authoritative owner of the saga state and the entry point for the frontend's checkout UX.

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
7. Request payment authorization via yc-payment (variant A or B per ADR-013)
8. Request order creation via yc-orders
9. Request payment capture
10. Honor the quoted price for the session TTL (ADR-011)
11. Drive compensation on any step failure
12. Resume from persisted state after a crash
13. Expire sessions on TTL and compensate

## Multi-Tenancy

- Sessions are tenant-scoped
- All sub-service calls carry the tenant via the access token

## Session Lifecycle

See the Checkout Saga definition in prior responses. States:

```
STARTED → PRICING → RESERVING → AUTHORIZING → ORDER_CREATE → CAPTURING → COMPLETED
                                                                      ↓
                        (any failure state) → COMPENSATING → FAILED
                                                           → EXPIRED
```

Variant B (combined auth+capture per ADR-013) replaces AUTHORIZING + CAPTURING with a single CHARGING state.

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
| POST | `/checkout/{session_id}/confirm` | Confirm checkout (triggers reserve → authorize → create → capture) | session owner |
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
3. All transitions are idempotent — repeating a step that already succeeded is a no-op (consumer-side idempotency via `idempotency_key`)

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
| `inventory.reservation_requested.v1` | Command → yc-inventory |
| `inventory.reservation_release_requested.v1` | Compensation command |
| `payment.authorization_requested.v1` | Command → yc-payment (variant A) |
| `payment.capture_requested.v1` | Command → yc-payment (variant A) |
| `payment.charge_requested.v1` | Command → yc-payment (variant B) |
| `payment.void_requested.v1` | Compensation command (variant A) |
| `payment.refund_requested.v1` | Compensation command (variant B — post-capture) |
| `orders.order_creation_requested.v1` | Command → yc-orders |
| `order.cancellation_requested.v1` | Compensation command |

## Subscribed Events

| Event | Action |
|---|---|
| `inventory.stock.reserved.v1` | Advance from RESERVING |
| `inventory.stock.reservation_failed.v1` | → COMPENSATING |
| `inventory.stock.reservation_expired.v1` | If session active and in later state: compensate from current state |
| `inventory.stock.reservation_released.v1` | Record compensation success |
| `payment.authorized.v1` | Advance from AUTHORIZING |
| `payment.authorization_failed.v1` | → COMPENSATING |
| `payment.captured.v1` | Advance from CAPTURING → COMPLETED |
| `payment.capture_failed.v1` | → COMPENSATING |
| `payment.charged.v1` | Variant B — advance to COMPLETED |
| `payment.charge_failed.v1` | → COMPENSATING |
| `payment.voided.v1` | Record compensation success |
| `payment.refund_completed.v1` | Record compensation success |
| `order.placed.v1` | Advance from ORDER_CREATE → CAPTURING |
| `order.creation_failed.v1` | → COMPENSATING |
| `order.cancelled.v1` | Record compensation success |
| `identity.user.deleted.v1` | Pseudonymize `user_id` on non-terminal sessions; delete `guest_contact` on terminal sessions beyond retention window |
| `platform.tenant.provisioned.v1` | Create `tenant_checkout_config` |
| `platform.tenant.deleted.v1` | Delete tenant sessions beyond legal retention |

## Compensation Matrix (recap)

| Failed Step | Must Undo |
|---|---|
| PRICING | Nothing |
| RESERVING | Nothing |
| AUTHORIZING | Release reservation (if any) |
| ORDER_CREATE | Void auth (or refund in Variant B), release reservation (if any) |
| CAPTURING | Cancel order, void auth (or refund in Variant B), release reservation (if any) |

Compensation is idempotent and ordered. All compensation events use idempotency keys based on `session_id + action`.

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
- **yc-orders** (order creation)
- **yc-preference** (display currency, locale — optional)

All inter-service calls go through yc-gateway with the caller's access token.

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
