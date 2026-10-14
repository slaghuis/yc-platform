# Checkout Saga

The checkout flow is the most complex orchestration in the platform. It involves Cart, Pricing, Inventory, Shipping, Tax, Payment, and Orders — any of which can fail at any step. We implement it as an **orchestrated saga** owned by a new service: **YC-Checkout**.

## Why a Dedicated YC-Checkout Service?

- **Orchestration logic doesn't belong in Cart** (Cart's job is cart content, not transaction coordination)
- **Doesn't belong in Orders either** — an order only exists if checkout succeeds
- Centralizes compensation logic (undoing on failure)
- Provides a stable API surface for the frontend during the multi-step checkout UX

## Saga Participants

| Service | Role | Compensation |
|---|---|---|
| YC-Cart | Source of items being checked out | — |
| YC-Pricing | Compute final line prices & totals | — (snapshot only) |
| YC-Tax | Compute taxes | — |
| YC-Shipping | Quote shipping | — |
| YC-Inventory | Reserve stock | Release reservation |
| YC-Payment | Authorize & capture payment | Void authorization / refund |
| YC-Orders | Create the order record | Cancel order |
| YC-Cart | Clear cart after success | Restore cart |

## State Machine

```
                 ┌─────────────┐
                 │   STARTED   │
                 └──────┬──────┘
                        │
                        ▼
                 ┌─────────────┐
                 │   PRICING   │────fail────┐
                 └──────┬──────┘            │
                        │                   │
                        ▼                   │
                 ┌─────────────┐            │
                 │  RESERVING  │────fail────┤
                 └──────┬──────┘            │
                        │                   │
                        ▼                   │
                 ┌─────────────┐            │
                 │ AUTHORIZING │────fail────┤
                 └──────┬──────┘            │
                        │                   │
                        ▼                   │
                 ┌─────────────┐            │
                 │ ORDER_CREATE│────fail────┤
                 └──────┬──────┘            │
                        │                   │
                        ▼                   │
                 ┌─────────────┐            │
                 │  CAPTURING  │────fail────┤
                 └──────┬──────┘            │
                        │                   │
                        ▼                   ▼
                 ┌─────────────┐     ┌─────────────┐
                 │  COMPLETED  │     │ COMPENSATING│
                 └─────────────┘     └──────┬──────┘
                                            │
                                            ▼
                                     ┌─────────────┐
                                     │   FAILED    │
                                     └─────────────┘
```

Every state transition is persisted to the YC-Checkout database AND emits an event. The saga is **resumable** — a crash in any state recovers from the persisted state.

## Checkout Session Model

```
CheckoutSession
├── session_id         UUID
├── tenant_id          TEXT
├── user_id            TEXT         (customer or visitor)
├── cart_id            UUID
├── state              ENUM
├── items              []LineItem   (snapshotted from cart at START)
├── pricing_snapshot   JSONB        (populated at PRICING)
├── tax_snapshot       JSONB        (populated at PRICING)
├── shipping_quote     JSONB        (populated at PRICING)
├── reservation_id     UUID         (populated at RESERVING)
├── reservation_expires TIMESTAMPTZ
├── payment_intent_id  TEXT         (populated at AUTHORIZING)
├── payment_auth_id    TEXT
├── order_id           UUID         (populated at ORDER_CREATE)
├── compensation_log   JSONB        (what has been undone, for recovery)
├── created_at         TIMESTAMPTZ
├── updated_at         TIMESTAMPTZ
└── expires_at         TIMESTAMPTZ  (session lifetime: 30 min)
```

## Happy Path — Event Flow

Correlation ID (`correlation_id`) ties every event in a single saga together. `causation_id` chains the parent-child relationship.

### Step 1: Start Checkout
Frontend calls `POST /checkout` on YC-Checkout.

```
YC-Checkout:
  1. Fetches cart from YC-Cart (synchronous, needs current state)
  2. Snapshots cart items into CheckoutSession
  3. Transitions STARTED
  4. Publishes: yc.<tenant>.checkout.checkout.started.v1
```

### Step 2: Price & Quote
```
YC-Checkout → YC-Pricing (sync call via gateway):
  - Request: items, user_id, currency (from Preference)
  - Response: priced line items, totals
YC-Checkout → YC-Tax (sync call):
  - Response: tax breakdown
YC-Checkout → YC-Shipping (sync call):
  - Response: available shipping options + rates
YC-Checkout:
  - Persists snapshots
  - Transitions PRICING
  - Publishes: yc.<tenant>.checkout.checkout.priced.v1
```

Frontend presents prices, shipping options, totals to the user. User confirms shipping method and address.

### Step 3: Reserve Inventory
Frontend calls `POST /checkout/{id}/confirm` with shipping choice and payment method.

```
YC-Checkout:
  1. Transitions RESERVING
  2. Publishes command: yc.<tenant>.inventory.reservation_requested.v1
     (correlation_id = session_id, causation_id = previous event)

YC-Inventory:
  1. Attempts to reserve all items atomically
  2. On success:
     - Creates reservation with TTL (default: 15 min)
     - Publishes: yc.<tenant>.inventory.stock.reserved.v1
  3. On failure (out of stock):
     - Publishes: yc.<tenant>.inventory.reservation_failed.v1

YC-Checkout (subscriber):
  - On stock.reserved: persist reservation_id, transition AUTHORIZING
  - On reservation_failed: → compensation flow
```

### Step 4: Authorize Payment
```
YC-Checkout:
  1. Publishes command: yc.<tenant>.payment.authorization_requested.v1
     Payload: amount, currency, payment_method_token, session_id

YC-Payment:
  1. Calls external gateway (Stripe, PayPal, etc.)
  2. On success: publishes yc.<tenant>.payment.authorized.v1
  3. On failure: publishes yc.<tenant>.payment.failed.v1

YC-Checkout:
  - On authorized: persist auth_id, transition ORDER_CREATE
  - On failed: → compensation flow
```

### Step 5: Create Order
```
YC-Checkout:
  1. Publishes command: yc.<tenant>.orders.order_creation_requested.v1
     Payload: full session snapshot

YC-Orders:
  1. Creates order record with status PENDING_CAPTURE
  2. Publishes: yc.<tenant>.order.order.placed.v1

YC-Checkout:
  - Persists order_id, transitions CAPTURING
```

### Step 6: Capture Payment
```
YC-Checkout:
  1. Publishes command: yc.<tenant>.payment.capture_requested.v1
     Payload: auth_id, order_id

YC-Payment:
  1. Captures the authorization
  2. On success: publishes yc.<tenant>.payment.captured.v1

YC-Checkout:
  - On captured: transitions COMPLETED
  - Publishes: yc.<tenant>.checkout.checkout.completed.v1
```

### Step 7: Post-Completion (fan-out, async)
Triggered by `order.placed.v1` and `payment.captured.v1`:

```
YC-Inventory:
  - On order.placed: commits reservation (decrements stock)
  - Publishes: yc.<tenant>.inventory.stock.committed.v1

YC-Cart:
  - On checkout.completed: clears the cart
  - Publishes: yc.<tenant>.cart.cleared.v1

YC-Message:
  - On order.placed: sends order confirmation via user's preferred channel

YC-Shipping:
  - On order.placed: begins fulfillment workflow (label generation, etc.)

YC-Analytics:
  - On checkout.completed, order.placed: records conversion
```

## Compensation Flow

When any step fails, YC-Checkout enters **COMPENSATING** state and undoes work in reverse order.

### Compensation Matrix

| Failed Step | Must Undo | Compensation Events |
|---|---|---|
| PRICING | Nothing | — |
| RESERVING | Nothing (nothing yet reserved on failure) | — |
| AUTHORIZING | Release reservation | `inventory.reservation_release_requested.v1` |
| ORDER_CREATE | Void auth, release reservation | `payment.void_requested.v1`, `inventory.reservation_release_requested.v1` |
| CAPTURING | Cancel order, void auth, release reservation | `order.cancellation_requested.v1`, `payment.void_requested.v1`, `inventory.reservation_release_requested.v1` |

### Compensation Rules

1. **Compensation events are commands**, same as forward events
2. Each compensation handler is **idempotent** — if a reservation was already released, the handler is a no-op
3. YC-Checkout tracks compensation progress in `compensation_log`
4. All compensations must complete before transitioning to **FAILED**
5. A compensation that fails is retried with exponential backoff
6. A compensation that cannot complete after N retries raises a **critical incident** — human intervention required (data in inconsistent state)

### Compensation Flow Example (ORDER_CREATE failed)

```
YC-Checkout state: COMPENSATING
compensation_log: []

Step 1: Publish yc.<tenant>.payment.void_requested.v1
  Wait for yc.<tenant>.payment.voided.v1 (or voided_failed.v1)
  On success: compensation_log += "payment_voided"

Step 2: Publish yc.<tenant>.inventory.reservation_release_requested.v1
  Wait for yc.<tenant>.inventory.stock.reservation_released.v1
  On success: compensation_log += "reservation_released"

Step 3: Publish yc.<tenant>.checkout.checkout.failed.v1
  Transition to FAILED
```

## Timeouts & Expiry

| Timeout | Duration | Action |
|---|---|---|
| Session total | 30 minutes | Auto-compensate from current state |
| Inventory reservation | 15 minutes | YC-Inventory publishes `reservation_expired.v1`; YC-Checkout compensates |
| Payment authorization | Gateway-specific (typ. 7 days) | If order creation fails after that, refund instead of void |
| Step response | 30 seconds | Retry; after 3 retries → compensation |

**Inventory reservation TTL is critical**: if the saga stalls between RESERVING and CAPTURING, the reservation must auto-expire so stock isn't locked forever. YC-Checkout must either complete or compensate before expiry.

## Idempotency

Every command event carries an **idempotency key** (typically `session_id` + step name):
- `authorization_requested` idempotency key: `{session_id}:authorize`
- `capture_requested` idempotency key: `{session_id}:capture`
- `reservation_requested` idempotency key: `{session_id}:reserve`

Consumers (Payment, Inventory, Orders) store processed idempotency keys and return the previous result on duplicate. This protects against:
- Outbox relay publishing twice
- Service crash between publish and state update
- Manual DLQ replay

## Visitor Checkout

A visitor can check out without registering:
1. YC-Identity has already issued a visitor token
2. Checkout proceeds normally, with `actor.type = "visitor"`
3. At the ORDER_CREATE step, YC-Orders prompts (via YC-Checkout response to the frontend) for an email address
4. YC-Identity promotes the visitor to a customer **only if the user opts in**; otherwise the order is stored with just the email
5. Post-order emails go to that email address via YC-Message

## Resumption After Crash

If YC-Checkout crashes mid-saga:
1. On restart, it queries all sessions in non-terminal states (STARTED, PRICING, RESERVING, AUTHORIZING, ORDER_CREATE, CAPTURING, COMPENSATING)
2. For each, it checks the last persisted state and the event history
3. Resumes forward from the persisted state (commands are idempotent)
4. Sessions older than their `expires_at` are moved to COMPENSATING

## Events Added to Taxonomy

Append to the Checkout/Order Domain:

| Event | Producer | Purpose |
|---|---|---|
| `checkout.started.v1` | YC-Checkout | Saga begins |
| `checkout.priced.v1` | YC-Checkout | Prices/quotes computed |
| `checkout.confirmed.v1` | YC-Checkout | User confirmed; moves to reserving |
| `checkout.completed.v1` | YC-Checkout | Saga success |
| `checkout.failed.v1` | YC-Checkout | Saga ended in compensation |
| `checkout.compensating.v1` | YC-Checkout | Entering compensation |
| `checkout.expired.v1` | YC-Checkout | Session timed out |
| `inventory.reservation_requested.v1` | YC-Checkout | Command → Inventory |
| `inventory.reservation_failed.v1` | YC-Inventory | Reservation declined |
| `inventory.reservation_release_requested.v1` | YC-Checkout | Compensation command |
| `payment.authorization_requested.v1` | YC-Checkout | Command → Payment |
| `payment.capture_requested.v1` | YC-Checkout | Command → Payment |
| `payment.void_requested.v1` | YC-Checkout | Compensation command |
| `payment.voided.v1` | YC-Payment | Compensation complete |
| `orders.order_creation_requested.v1` | YC-Checkout | Command → Orders |
| `order.cancellation_requested.v1` | YC-Checkout | Compensation command |

