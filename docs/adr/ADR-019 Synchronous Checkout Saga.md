# ADR-019: Synchronous Checkout Saga

**Status:** Accepted
**Date:** 2026-10-14
**Context:** The checkout saga was documented two ways at once. `docs/diagrams/checkout-saga.md`, `yc-checkout.md`, ADR-013 and `event-conventions.md` drove it with command events on NATS (`payment.authorization_requested.v1`, `orders.order_creation_requested.v1`, …), while every participant's spec also defined synchronous `/internal/` endpoints for the same steps. The confirm step is the payment process: the customer is waiting for the answer. With command events, every step pays the outbox relay delay plus consumer delivery, the saga needs a subscription and a response timeout per step, and the frontend has to poll for the outcome. Separately, `order.placed.v1` was published when the order was created, before payment was captured, so shipping, messaging and inventory could act on an order that was never paid. And no step committed the inventory reservation: yc-inventory exposes `POST /internal/reservations/{id}/commit` but nothing called it, so reservations would simply expire.

## Decision

**yc-checkout orchestrates the saga with synchronous HTTP calls to each participant's `/internal/` endpoints.** The message bus carries only facts (`checkout.*.v1`, `order.placed.v1`, …) and the few asynchronous inputs listed in rule 9. The saga uses no command events.

Orders are created as **`pending_payment`** and confirmed only after payment succeeds, so `order.placed.v1` is published only for paid orders.

## Rules

1. **Calls.** yc-checkout calls `/internal/` endpoints through yc-gateway with its service token, propagating `X-YC-Tenant-ID`, `X-YC-Actor-ID`, `X-YC-Actor-Type` and `X-YC-Correlation-ID` (`api-conventions.md` §9).

   | Step | Forward call | Compensation call |
   |---|---|---|
   | PRICING | yc-cart `GET /internal/carts/{user_id}/{type}`, yc-pricing `POST /internal/evaluate`, yc-shipping `POST /internal/quote` | — |
   | RESERVING | yc-inventory `POST /internal/reservations` | `POST /internal/reservations/{id}/release` |
   | AUTHORIZING (A) | yc-payment `POST /internal/authorize` | `POST /internal/void` |
   | ORDER_CREATE | yc-orders `POST /internal/orders` (creates `pending_payment`) | `POST /internal/orders/{id}/abandon` |
   | CAPTURING (A) | yc-payment `POST /internal/capture` | — (see rule 6) |
   | CHARGING (B) | yc-payment `POST /internal/charge` | — (see rule 6) |
   | CONFIRMING | yc-orders `POST /internal/orders/{id}/confirm`, then yc-inventory `POST /internal/reservations/{id}/commit` (if a reservation exists) | none — forward only |

2. **Step order.**
   - Variant A (separate auth and capture): `STARTED → PRICING → RESERVING → AUTHORIZING → ORDER_CREATE → CAPTURING → CONFIRMING → COMPLETED`
   - Variant B (combined charge, ADR-013): `STARTED → PRICING → RESERVING → ORDER_CREATE → CHARGING → CONFIRMING → COMPLETED`

   RESERVING is skipped when no SKU is tracked (ADR-012). Any failure before payment succeeds moves the session to `COMPENSATING`, then `FAILED`.

3. **Orders stay pending until paid.** `POST /internal/orders` creates the order with status `pending_payment` and publishes nothing. `POST /internal/orders/{id}/confirm` moves it to `received` and publishes `order.placed.v1`. `POST /internal/orders/{id}/abandon` moves it to `abandoned`, which is terminal, invisible to customers and shop admins, and publishes nothing. Because the order exists before the charge, Variant B never has to refund to compensate a failed order creation.

4. **Persist around every call.** The session records the step before the call is made and the result after it returns. After a crash, resumption finds the session in a known step and repeats that call.

5. **Idempotency.** Every mutating call carries `Idempotency-Key: {session_id}:{step}` (compensation calls use `{session_id}:{step}:undo`). Participants store the key with their response and return the stored response when the key repeats (`api-conventions.md` §10). This makes retries and crash resumption safe, and resolves calls whose outcome is unknown because they timed out. For payment calls that ended `pending` (rule 8), yc-payment answers a repeated key with the transaction's current status rather than the stored `pending`.

6. **Timeouts, retries and the point of no return.**
   - Each call has a timeout (default 10 s; payment calls 35 s, above the gateway timeout of 30 s).
   - Network errors, timeouts and 5xx responses are retried with the same key, up to 3 attempts with backoff.
   - A business rejection (4xx: out of stock, card declined) is not retried; the saga compensates.
   - When retries run out before payment has succeeded, the saga compensates. Compensation calls treat "nothing to undo" as success (for example, a void for a key that never produced an authorization).
   - **Once capture or charge succeeds, the saga never compensates.** CONFIRMING is retried by the background sweeper until yc-orders has confirmed the order and yc-inventory has committed the reservation; a session in CONFIRMING for more than 5 minutes raises a critical alert. Refunds after this point are an order action in yc-orders, not saga compensation.
   - yc-inventory commits a reservation even if it expired in the meantime (payment succeeded close to the TTL). If that takes available stock below zero, it records the oversell for the shop admin rather than failing.

7. **Confirm response.** `POST /checkout/{session_id}/confirm` runs the saga inline and returns the session:
   - `200` with state `COMPLETED` and the `order_id`, or
   - `422` with the failure reason once compensation has finished (`FAILED`), or
   - `202` with the current state if the request deadline (60 s) passes, or compensation is still retrying in the background, or payment needs customer action (rule 8). The frontend then polls `GET /checkout/{session_id}`.

8. **Payments that need customer action or settle later** (3-D Secure, delayed gateway confirmation). yc-payment returns `pending` with the gateway's `next_action`. yc-checkout leaves the session in AUTHORIZING or CHARGING and returns `202` with `next_action`. The session resumes when either:
   - the frontend calls `POST /checkout/{session_id}/confirm` again after the customer completes the action (the repeated idempotency key returns the now-final result), or
   - yc-payment publishes the outcome from the gateway webhook (rule 9).

9. **Asynchronous inputs to yc-checkout.** The saga subscribes only to:
   - `payment.authorized.v1`, `payment.authorization_failed.v1`, `payment.charged.v1`, `payment.charge_failed.v1`, `payment.captured.v1`, `payment.capture_failed.v1` — used only for sessions waiting on a `pending` payment
   - `inventory.stock.reservation_expired.v1` — compensate if the session has not yet passed the point of no return
   - `identity.user.deleted.v1`, `platform.tenant.provisioned.v1`, `platform.tenant.deleted.v1`

10. **Events.** yc-checkout publishes only its own facts (`checkout.*.v1`) through the outbox (ADR-006). These saga command events are withdrawn and get no schemas: `inventory.reservation_requested.v1`, `inventory.reservation_release_requested.v1`, `payment.authorization_requested.v1`, `payment.capture_requested.v1`, `payment.charge_requested.v1`, `payment.void_requested.v1`, `payment.refund_requested.v1`, `orders.order_creation_requested.v1`, `order.cancellation_requested.v1`. `order.creation_failed.v1` is also withdrawn; the failure is the HTTP response.

11. **TTLs are unchanged.** Session TTL (ADR-011) and reservation TTL (ADR-012) still apply. The expiry sweeper compensates expired sessions that have not reached the point of no return.

## Consequences

**Positive**
- The customer gets the outcome in the confirm response; polling is needed only for 3-D Secure, delayed settlement or slow compensation.
- Fewer moving parts: no per-step subscriptions or response timeouts on the bus, and nine command event types removed from the taxonomy.
- Downstream services (shipping, messaging, analytics) only ever see paid orders, and stock is committed exactly when payment succeeds.
- Variant B no longer refunds to compensate a failed order creation (ADR-013's "worse user experience" case disappears).

**Negative**
- Temporal coupling: confirm needs cart, pricing, shipping, inventory, payment and orders to be available. Checkout cannot complete without them anyway, but an outage now fails the request instead of delaying it.
- Every participant must implement `Idempotency-Key` storage on its internal mutating endpoints.
- Confirm latency is the sum of the steps, dominated by the payment gateway. yc-checkout holds the request open for up to 60 s.

## Alternatives Considered

- **Command events on the bus** (the earlier documents): rejected. The customer waits on the result either way, so the bus adds latency, a polling UX and a subscription per step without adding resilience the idempotent calls don't already give.
- **Choreography** (each service reacts to the previous service's event): rejected. No single owner of compensation, and the flow is hard to follow and test.

## Documents Amended

- ADR-012 (Events Affected), ADR-013 (Saga Impact): command events replaced by the internal calls above.
- `docs/diagrams/checkout-saga.md`, `docs/services/yc-checkout.md`, `yc-orders.md`, `yc-payment.md`, `yc-inventory.md`, `yc-shipping.md`, `docs/standards/event-conventions.md` §2 and §10, `docs/implementation-plan.md` Phase 4.
