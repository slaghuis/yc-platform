# Checkout Saga

The checkout flow is the most complex orchestration in the platform. It involves Cart, Pricing, Shipping, Inventory, Payment and Orders — any of which can fail at any step. It is an **orchestrated saga** owned by **yc-checkout**, which calls each participant **synchronously** (ADR-019). The bus carries only the facts that result.

## Why a Dedicated yc-checkout Service?

- **Orchestration logic doesn't belong in Cart** (Cart's job is cart content, not transaction coordination)
- **Doesn't belong in Orders either** — an order only becomes real once it is paid
- Centralizes compensation logic (undoing on failure)
- Provides a stable API surface for the frontend during the multi-step checkout UX

## Saga Participants

| Service | Role | Internal call | Compensation |
|---|---|---|---|
| yc-cart | Source of items being checked out | `GET /internal/carts/{user_id}/{type}` | — |
| yc-pricing | Final line prices, totals and tax (ADR-016) | `POST /internal/evaluate` | — (snapshot only) |
| yc-shipping | Shipping quotes | `POST /internal/quote` | — |
| yc-inventory | Reserve stock (tracked SKUs only, ADR-012), commit once paid | `POST /internal/reservations`, `/internal/reservations/{id}/commit` | `POST /internal/reservations/{id}/release` |
| yc-payment | Authorize & capture (A) or charge (B) | `POST /internal/authorize`, `/capture`, `/charge` | `POST /internal/void` |
| yc-orders | Create pending order, confirm once paid | `POST /internal/orders`, `/internal/orders/{id}/confirm` | `POST /internal/orders/{id}/abandon` |

The cart is cleared asynchronously when yc-cart receives `checkout.completed.v1`.

## State Machine

Variant A (gateway supports separate authorize and capture):

```
STARTED → PRICING → RESERVING → AUTHORIZING → ORDER_CREATE → CAPTURING → CONFIRMING → COMPLETED
```

Variant B (combined charge, ADR-013):

```
STARTED → PRICING → RESERVING → ORDER_CREATE → CHARGING → CONFIRMING → COMPLETED
```

Any failure **before** capture or charge succeeds:

```
<failed step> → COMPENSATING → FAILED
<any pre-payment step> --(session TTL reached)--> COMPENSATING → EXPIRED
```

RESERVING is skipped when no SKU is tracked. **Capture or charge success is the point of no return**: after it, the saga only moves forward, and CONFIRMING is retried until yc-orders confirms.

Every state transition is persisted to the yc-checkout database before and after each call, and emits a `checkout.*.v1` fact where one is listed below. The saga is **resumable** — a crash in any state recovers from the persisted state by repeating the in-flight call with the same idempotency key.

## Checkout Session Model

```
CheckoutSession
├── session_id         UUID
├── tenant_id          TEXT
├── user_id            TEXT         (customer or visitor)
├── cart_id            UUID
├── state              ENUM
├── items              []LineItem   (snapshotted from cart at START)
├── pricing_snapshot   JSONB        (populated at PRICING; includes tax)
├── shipping_quote     JSONB        (populated at PRICING)
├── reservation_id     UUID         (populated at RESERVING)
├── reservation_expires TIMESTAMPTZ
├── payment_intent_id  TEXT         (populated at AUTHORIZING / CHARGING)
├── payment_auth_id    TEXT
├── order_id           UUID         (populated at ORDER_CREATE)
├── compensation_log   JSONB        (what has been undone, for recovery)
├── created_at         TIMESTAMPTZ
├── updated_at         TIMESTAMPTZ
└── expires_at         TIMESTAMPTZ  (session lifetime: 30 min, ADR-011)
```

## Happy Path (Variant A)

`correlation_id` (from `X-YC-Correlation-ID`) ties every call and every event of one checkout together. All calls go through yc-gateway with yc-checkout's service token.

### Step 1: Start checkout
Frontend calls `POST /checkout`.

```
yc-checkout:
  1. GET yc-cart /internal/carts/{user_id}/{type}
  2. Snapshots cart items into the session
  3. Transitions STARTED
  4. Publishes checkout.started.v1
```

### Step 2: Price and quote

```
yc-checkout:
  1. POST yc-pricing /internal/evaluate   → priced lines, totals, tax breakdown
  2. POST yc-shipping /internal/quote     → shipping options and rates (if requires_shipping)
  3. Persists snapshots, transitions PRICING
  4. Publishes checkout.priced.v1
```

The frontend shows prices, shipping options and totals. The customer picks a shipping option (`POST /checkout/{id}/shipping`).

### Step 3: Confirm
Frontend calls `POST /checkout/{id}/confirm` with guest contact (visitors), delivery address, shipping choice and the gateway token. yc-checkout validates the guest data first (ADR-010), publishes `checkout.confirmed.v1`, then runs the remaining steps inline:

```
RESERVING     POST yc-inventory /internal/reservations          Idempotency-Key: {session_id}:reserve
AUTHORIZING   POST yc-payment   /internal/authorize             Idempotency-Key: {session_id}:authorize
ORDER_CREATE  POST yc-orders    /internal/orders                Idempotency-Key: {session_id}:order_create
              (order created as pending_payment; no event)
CAPTURING     POST yc-payment   /internal/capture               Idempotency-Key: {session_id}:capture
              ── point of no return ──
CONFIRMING    POST yc-orders    /internal/orders/{id}/confirm   Idempotency-Key: {session_id}:confirm
              (order → received; yc-orders publishes order.placed.v1)
              POST yc-inventory /internal/reservations/{id}/commit   Idempotency-Key: {session_id}:commit
              (yc-inventory publishes inventory.stock.committed.v1)
COMPLETED     yc-checkout publishes checkout.completed.v1
```

The confirm request returns `200` with the `order_id`. Variant B replaces AUTHORIZING and CAPTURING with a single CHARGING call placed after ORDER_CREATE:

```
RESERVING     POST yc-inventory /internal/reservations          Idempotency-Key: {session_id}:reserve
ORDER_CREATE  POST yc-orders    /internal/orders                Idempotency-Key: {session_id}:order_create
CHARGING      POST yc-payment   /internal/charge                Idempotency-Key: {session_id}:charge
              ── point of no return once captured ──
CONFIRMING    (as above)
```

The variant comes from the gateway yc-payment routes the store currency to when the session starts (`GET /internal/gateway-route`, ADR-013); the customer may pick another enabled gateway at confirm. A hosted gateway (Yoco) needs no token: CHARGING returns `202` with a `redirect_to_url` `next_action`, and the outcome arrives by webhook.

### Step 4: Post-completion fan-out (asynchronous)

Triggered by `order.placed.v1` and `checkout.completed.v1`:

```
yc-cart:       on checkout.completed  → clears the cart, publishes cart.cleared.v1
yc-message:    on order.placed        → order confirmation via the customer's preferred channel
yc-shipping:   on order.placed        → starts fulfillment (if requires_shipping)
yc-analytics:  on checkout.completed, order.placed → records the conversion
```

## Payment Needing Customer Action

For 3-D Secure or gateways that confirm later, yc-payment returns `pending` with the gateway's `next_action`. yc-checkout keeps the session in AUTHORIZING (or CHARGING) and returns `202` with `next_action`. The saga continues when:
- the frontend calls `POST /checkout/{id}/confirm` again after the customer completes the action (the same idempotency key now returns the final result), or
- yc-payment publishes `payment.authorized.v1` / `payment.authorization_failed.v1` (or the charge and capture equivalents) from the gateway webhook.

## Compensation Flow

When a step fails before the point of no return, yc-checkout enters **COMPENSATING** and undoes completed work in reverse order, using synchronous calls.

### Compensation Matrix

| Failed Step | Must Undo | Compensation calls |
|---|---|---|
| PRICING | Nothing | — |
| RESERVING | Nothing (nothing reserved on failure) | — |
| AUTHORIZING (A) | Release reservation | inventory release |
| ORDER_CREATE | Void auth (A), release reservation | payment void (A), inventory release |
| CAPTURING (A) | Abandon order, void auth, release reservation | orders abandon, payment void, inventory release |
| CHARGING (B) | Void the pending charge, abandon order, release reservation | payment void (a hosted charge is left open), orders abandon, inventory release |
| CONFIRMING | Never compensated — retried until it succeeds | — |

A hosted charge cannot be cancelled, so the customer can still pay it after the session has failed, expired or been cancelled. yc-checkout then refunds it in full on `payment.charged.v1` (`POST /internal/refund`, key `{session_id}:charge:refund`) and logs `late_charge_refunded` (ADR-019 rule 12).

### Compensation Rules

1. Compensation calls carry `Idempotency-Key: {session_id}:{step}:undo`.
2. Each compensation endpoint is **idempotent** and treats "nothing to undo" as success (a release of an already-released reservation, a void for a key that never authorized).
3. yc-checkout records progress in `compensation_log`.
4. All compensations must complete before the session moves to **FAILED** (or **EXPIRED**).
5. A compensation that fails is retried with exponential backoff by the background sweeper.
6. A compensation that cannot complete after N retries raises a **critical incident** — human intervention required.

### Example: CAPTURING failed (Variant A)

```
state: COMPENSATING, compensation_log: []

1. POST yc-orders    /internal/orders/{id}/abandon        → compensation_log += "order_abandoned"
2. POST yc-payment   /internal/void                       → compensation_log += "payment_voided"
3. POST yc-inventory /internal/reservations/{id}/release  → compensation_log += "reservation_released"
4. Publish checkout.failed.v1, transition FAILED
```

The confirm request returns `422` with the failure reason once compensation has finished, or `202` if compensation is still retrying in the background.

## Timeouts & Expiry

| Timeout | Duration | Action |
|---|---|---|
| Session total | 30 minutes (ADR-011) | Compensate from the current state (if before the point of no return) |
| Inventory reservation | 15 minutes | yc-inventory publishes `inventory.stock.reservation_expired.v1`; yc-checkout compensates if the session hasn't passed the point of no return |
| Payment authorization | Gateway-specific (typ. 7 days) | Not reached in a live session |
| Internal call | 10 s (payment: 35 s) | Retry with the same key, 3 attempts; then compensate |
| Confirm request | 60 s | Return `202` with the current state; the saga continues in the background |
| CONFIRMING | 5 minutes | Keep retrying; raise a critical alert. A reservation that expired meanwhile is still committed (ADR-019 rule 6) |

**Inventory reservation TTL is critical**: if the saga stalls between RESERVING and capture, the reservation must auto-expire so stock isn't locked forever.

## Idempotency

Every mutating internal call carries `Idempotency-Key: {session_id}:{step}`:
- `{session_id}:reserve`, `{session_id}:authorize`, `{session_id}:order_create`, `{session_id}:capture` (or `:charge`), `{session_id}:confirm`, `{session_id}:commit`
- Compensation: the forward key with `:undo` appended

Participants (Inventory, Payment, Orders) store processed keys with the response and return the stored response on a repeat. This protects against:
- Retries after timeouts where the first call actually succeeded
- yc-checkout crashing between the call and persisting the result
- The frontend repeating `POST /checkout/{id}/confirm`

## Visitor Checkout

A visitor can check out without registering:
1. yc-identity has already issued a visitor token
2. Checkout proceeds normally, with `actor.type = "visitor"`
3. The confirm request carries the guest contact (email, phone) and delivery address; yc-checkout validates them before reserving stock (ADR-010)
4. yc-identity promotes the visitor to a customer **only if the user opts in**; otherwise the order is stored with just the guest contact
5. Post-order messages go to that email address via yc-message

## Resumption After Crash

If yc-checkout crashes mid-saga:
1. On restart, it queries all sessions in non-terminal states
2. For each, it repeats the in-flight call from the persisted state with the same idempotency key
3. Sessions past `expires_at` and before the point of no return move to COMPENSATING
4. Sessions in CONFIRMING are always driven forward

## Events in the Taxonomy

Published by yc-checkout:

| Event | Purpose |
|---|---|
| `checkout.started.v1` | Saga begins |
| `checkout.priced.v1` | Prices/quotes computed |
| `checkout.shipping_selected.v1` | Shipping option chosen |
| `checkout.confirmed.v1` | User confirmed; saga runs |
| `checkout.validation_failed.v1` | Confirm rejected (missing data) |
| `checkout.compensating.v1` | Entering compensation |
| `checkout.completed.v1` | Saga success |
| `checkout.failed.v1` | Saga ended in compensation |
| `checkout.expired.v1` | Session timed out |

No command events are used (ADR-019). Payment, inventory and order facts are listed under their own domains.
