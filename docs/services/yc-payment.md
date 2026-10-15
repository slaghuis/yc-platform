# yc-payment — Service Requirement

## Overview

`yc-payment` is a gateway-agnostic abstraction over external payment providers. It ships with two drivers in v1: **Yoco** (primary for South Africa) and **Stripe** (international). It does not store card data (ADR-009); it handles gateway tokens, authorizations, captures, voids, refunds, and inbound webhooks. It operates in **store currency only** (ADR-014).

## Location in Monorepo

```
/services/yc-payment/
```

## Responsibilities

1. Expose a gateway-agnostic interface for payment operations
2. Implement a **Yoco driver** and a **Stripe driver**
3. Authorize payments (Variant A gateways)
4. Capture authorized payments (Variant A)
5. Charge in a single step (Variant B)
6. Void authorizations
7. Refund captured payments (full or partial)
8. Handle gateway webhooks (asynchronous status updates)
9. Store payment metadata (gateway reference IDs, status, amounts) — **not card data**
10. Encrypt and store per-tenant gateway credentials
11. Emit payment lifecycle events for yc-checkout and yc-orders

## Multi-Tenancy

- All payments are tenant-scoped
- Each tenant configures their own gateway credentials
- Gateway credentials are encrypted at rest using a KMS or app-layer envelope encryption

## Gateway Driver Interface (Recap from ADR-013)

```go
type GatewayDriver interface {
    Name() string
    SupportedCurrencies() []string
    SupportsSeparateAuthCapture() bool

    Authorize(ctx, AuthorizeRequest)       (AuthorizeResponse, error)
    Capture(ctx, CaptureRequest)           (CaptureResponse, error)
    Charge(ctx, ChargeRequest)             (ChargeResponse, error)       // combined
    Void(ctx, VoidRequest)                 (VoidResponse, error)
    Refund(ctx, RefundRequest)             (RefundResponse, error)
    ParseWebhook(ctx, WebhookRequest)      (WebhookEvent, error)
}
```

### Driver Capabilities (v1)

| Driver | Supports Separate Auth+Capture | Notes |
|---|---|---|
| Stripe | **Yes** (Variant A) | Payment Intents API with `capture_method: manual` |
| Yoco | **No** (Variant B) | Online Checkout / Payments API — combined charge; refunds supported |

When Yoco is chosen, yc-checkout uses the Variant B flow (charge instead of auth+capture). The order is created before the charge (ADR-019), so a failed order creation moves no money; only a hosted payment completed after checkout gave up on it is refunded (ADR-019 rule 12).

## Transaction States

```
authorized ─▶ captured ─▶ (final)
    │             │
    │             └─▶ refunded (partial or full)
    │
    └─▶ voided (final)

charged (Variant B) ─▶ (final)
                    └─▶ refunded (partial or full)

failed (final)
```

Every state transition is persisted and emitted as an event.

## Webhook Handling

Each gateway sends asynchronous webhooks for:
- Delayed authorization success (3D Secure challenges, bank callbacks)
- Capture confirmations (some gateways settle asynchronously)
- Refund confirmations
- Chargebacks and disputes

### Webhook Flow

```
Gateway → yc-payment: POST /webhooks/{gateway}
  1. Verify signature with tenant's webhook secret
  2. Parse via driver's ParseWebhook
  3. Match to local transaction by gateway reference
  4. Update local state, publish corresponding event
  5. Return 200 immediately (gateways retry on non-2xx)
  6. On signature failure: 401, log, publish security alert
```

**Idempotency**: webhooks may be delivered multiple times. yc-payment dedupes by `(gateway, gateway_event_id)`.

## Gateway Credentials Storage

- Credentials stored encrypted per tenant
- Encryption key from env var (KMS integration future scope)
- Stored fields: `api_key`, `webhook_secret`, `public_key` (for frontend SDK), driver-specific extras
- Rotation supported: new credentials can be stored alongside old; cutover is a tenant action

## Chargebacks

- Gateway webhooks notify of chargebacks
- yc-payment publishes `payment.chargeback_received.v1`
- yc-orders subscribes and places the order in a `disputed` state
- yc-message notifies the shop admin
- v1: tracking only; dispute response workflow is manual (via gateway dashboard)

## API

### Public Endpoints

None. Payment capture flows through yc-checkout; the frontend talks directly to the gateway SDK for card entry.

### Internal Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| POST | `/internal/authorize` | Authorize (Variant A) | service |
| POST | `/internal/capture` | Capture (Variant A) | service |
| POST | `/internal/charge` | Charge (Variant B) | service |
| POST | `/internal/void` | Void authorization | service |
| POST | `/internal/refund` | Refund captured payment | service |
| GET | `/internal/transactions/{id}` | Fetch transaction | service |
| GET | `/internal/gateway-capabilities/{gateway}` | Capability lookup for yc-checkout | service |
| GET | `/internal/gateway-route?currency=&gateway=` | The shop's gateway for a currency, with its capabilities (ADR-013) | service |

yc-checkout calls these synchronously during the saga (ADR-019):
- Mutating calls require `Idempotency-Key` (`{session_id}:{step}`, compensation `{session_id}:{step}:undo`), stored in `transactions.idempotency_key`. A repeated key returns the transaction's **current** status, so a retry after a timeout or a `pending` result gets the real outcome.
- When the gateway needs customer action (3-D Secure) or confirms later, the call returns `pending` with the gateway's `next_action`. The final outcome arrives by webhook and is published as `payment.authorized.v1`, `payment.charged.v1`, `payment.captured.v1` or the matching `*_failed.v1`.
- Void and refund treat "nothing to undo" (no transaction for the key) as success.

### Webhook Endpoints (Public, Signed)

| Method | Path | Description |
|---|---|---|
| POST | `/webhooks/stripe` | Stripe webhook |
| POST | `/webhooks/yoco` | Yoco webhook |

### Admin Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| GET | `/admin/transactions` | List transactions (filterable) | shop_admin |
| GET | `/admin/transactions/{id}` | Transaction detail | shop_admin |
| POST | `/admin/transactions/{id}/refund` | Manual refund | shop_admin |
| GET | `/admin/gateway-config` | Get gateway config (credentials masked) | shop_admin |
| PUT | `/admin/gateway-config` | Set gateway credentials | shop_admin |
| POST | `/admin/gateway-config/test` | Test connectivity to gateway | shop_admin |

### Phase 4 and 5 Implementation Notes

The contract is `services/yc-payment/api/openapi.yaml`. Implemented: the Stripe driver (Variant A), the Yoco driver (Variant B), the internal authorize, capture, charge, void, refund, transaction, capability and route endpoints, both webhooks, the admin transaction views, gateway settings with routing, Stripe chargebacks and the user-deleted subscription (ADR-025). Not yet: `POST /admin/transactions/{id}/refund` (refunds go through yc-orders' admin cancel), Yoco disputes and the tenant subscriptions.

- **Stripe driver** is plain HTTP against the PaymentIntents API (no stripe-go): authorize creates a manual-capture PaymentIntent, capture and void are `capture` and `cancel`, and mutating calls carry Stripe's own `Idempotency-Key`. `stripetest` is an in-memory fake of that surface for tests; `cmd/fake-stripe` serves it on a port for the dev stack (`YC_PAYMENT_STRIPE_API_BASE`), and it honours `pm_card_visa`, `pm_card_chargeDeclined` and `pm_card_threeDSecure2Required`.
- **Gateway settings** are per gateway: `PUT /admin/gateway-config/{gateway}` and `POST /admin/gateway-config/{gateway}/test`. The API key and webhook secret are sealed with AES-256-GCM under `YC_PAYMENT_CIPHER_KEY` (base64 of 32 bytes) and only ever returned as a hint (`sk_test_…4242`).
- **Outcomes, not errors.** A declined authorisation is `200` with status `failed` and a `failure_reason`; 3-D Secure is `200` with status `pending` and the gateway's `next_action`. A refused capture is `400 payment_declined`; a gateway outage is `503 gateway_unavailable`; a shop without the gateway set up is `412`.
- **Idempotency is native**, not the shared middleware: authorize and refund look up the transaction by `Idempotency-Key` and return its current state, so a retry after a timeout or a pending result learns the real outcome. Capture and void are idempotent by state. Void names the transaction or, for a caller that never learnt its ID, the authorize key; nothing to void is success, and a captured payment is `409`.
- **Webhooks** (`POST /webhooks/stripe`, `POST /webhooks/yoco`) are gateway callback routes on the shop's host (ADR-020): no token; the body is verified with the shop's webhook secret. Stripe uses `Stripe-Signature` (5-minute tolerance); Yoco uses Standard Webhooks (`webhook-id`, `webhook-timestamp`, `webhook-signature`, a `whsec_` secret, 3-minute tolerance). Payment and refund events update the transaction and publish the matching event; anything verified gets `200`, a bad signature `401` and `payment.webhook_signature_failed.v1`. Each event is recorded in `webhook_events` by `(gateway, event ID)`, so a redelivery is acknowledged without effect. Locally, forward with `stripe listen --forward-to localhost:8090/webhooks/stripe -H "Host: demo-shop.localhost"`.
- **Yoco driver** is plain HTTP against the Checkout API. A charge creates a hosted checkout (`POST /api/checkouts`, the transaction's ID as `externalId`) and is `pending` with `next_action` `redirect_to_url`; it takes no payment token and needs a `return_url`. The outcome arrives only by webhook (`payment.succeeded` or `payment.failed`, naming the checkout in `metadata.checkoutId`) and is published as `payment.charged.v1` or `payment.charge_failed.v1`. The API can neither look up nor cancel a checkout, so a void leaves it `pending` (still payable), and refunds go through `POST /api/checkouts/{id}/refund`. Only ZAR. `yocotest` is an in-memory fake that signs and delivers webhooks; `cmd/fake-yoco` serves it for the dev stack (`YC_PAYMENT_YOCO_API_BASE`), with a `_fake` page to pay or fail a checkout.
- **Routing** (ADR-013). Each gateway config has a `priority` (0–1000, lowest first, default 100) and an optional `currencies` list (empty: all the driver takes; a currency the driver cannot take is `400`). `GET /internal/gateway-route?currency=` returns the enabled gateway with the lowest priority that takes the currency, with its capabilities (`supports_separate_auth_capture`, `hosted_payment`, `public_key`); `&gateway=` checks a named one. No such gateway is `412`.
- **Charge** (`POST /internal/charge`) is idempotent by `Idempotency-Key` like authorize, records `kind = charge` and the order it pays for, and sends the gateway the tenant, session and transaction IDs as metadata.

### Phase 8 Implementation Notes

- **Chargebacks** (ADR-025 rule 17): a verified Stripe `charge.dispute.created` webhook is stored in `chargebacks` once per dispute and published as `payment.chargeback_received.v1` (transaction, order, amount, reason, gateway reference). yc-orders marks the order disputed; yc-monitor alerts the shop; yc-audit records it. Yoco disputes and dispute outcomes are not handled. The dev stack's fake Stripe opens a dispute with `POST /_fake/payment_intents/{id}/dispute` (`webhook_url`, `webhook_secret`, `reason`).
- **GDPR:** yc-payment subscribes to `identity.user.deleted.v1`; see GDPR Behavior.

## Database Schema

Database: **`yc_payment`**

```sql
CREATE TABLE tenant_gateway_config (
    tenant_id          TEXT NOT NULL,
    gateway            TEXT NOT NULL,
    enabled            BOOLEAN NOT NULL DEFAULT TRUE,
    credentials_enc    BYTEA NOT NULL,          -- encrypted JSON blob
    public_key         TEXT,                     -- for frontend SDK
    webhook_secret_enc BYTEA NOT NULL,
    priority           INT NOT NULL DEFAULT 100,  -- 0..1000, lowest routes first
    currencies         TEXT[] NOT NULL DEFAULT '{}', -- empty: all the driver takes
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, gateway)
);

CREATE TABLE transactions (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           TEXT NOT NULL,
    session_id          TEXT,                     -- yc-checkout session_id
    order_id            UUID,
    gateway             TEXT NOT NULL,
    gateway_reference   TEXT NOT NULL,            -- gateway's charge/intent id
    status              TEXT NOT NULL,            -- 'authorized','captured','charged','voided','refunded','failed'
    amount_minor        BIGINT NOT NULL,          -- store currency
    currency            TEXT NOT NULL,
    captured_amount_minor BIGINT NOT NULL DEFAULT 0,
    refunded_amount_minor BIGINT NOT NULL DEFAULT 0,
    idempotency_key     TEXT NOT NULL,
    gateway_metadata    JSONB NOT NULL DEFAULT '{}',
    failure_reason      TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, idempotency_key),
    UNIQUE (gateway, gateway_reference)
);
CREATE INDEX idx_transactions_tenant ON transactions (tenant_id);
CREATE INDEX idx_transactions_order ON transactions (order_id);

CREATE TABLE refunds (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    transaction_id  UUID NOT NULL REFERENCES transactions(id),
    tenant_id       TEXT NOT NULL,
    amount_minor    BIGINT NOT NULL,
    reason          TEXT,
    gateway_reference TEXT,
    status          TEXT NOT NULL,              -- 'pending','completed','failed'
    actor_id        TEXT,
    idempotency_key TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at    TIMESTAMPTZ,
    UNIQUE (tenant_id, idempotency_key)
);

CREATE TABLE webhook_events (
    gateway             TEXT NOT NULL,
    gateway_event_id    TEXT NOT NULL,
    tenant_id           TEXT NOT NULL,
    received_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    processed_at        TIMESTAMPTZ,
    payload             JSONB NOT NULL,
    PRIMARY KEY (gateway, gateway_event_id)
);

CREATE TABLE chargebacks (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    transaction_id  UUID NOT NULL REFERENCES transactions(id),
    tenant_id       TEXT NOT NULL,
    gateway_reference TEXT NOT NULL,
    amount_minor    BIGINT NOT NULL,
    reason_code     TEXT,
    status          TEXT NOT NULL,              -- 'open','won','lost'
    received_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at     TIMESTAMPTZ
);

CREATE TABLE outbox ( ... );
CREATE TABLE processed_events ( ... );

ALTER TABLE transactions ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON transactions
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform');
-- similar on all tenant-scoped tables
```

## Published Events

| Event | When |
|---|---|
| `payment.intent.created.v1` | Transaction record created, before gateway call |
| `payment.authorized.v1` | Authorization succeeded (Variant A) |
| `payment.authorization_failed.v1` | Authorization declined |
| `payment.captured.v1` | Capture succeeded (Variant A) |
| `payment.capture_failed.v1` | Capture declined |
| `payment.charged.v1` | Combined charge succeeded (Variant B) |
| `payment.charge_failed.v1` | Combined charge declined |
| `payment.voided.v1` | Authorization voided |
| `payment.void_failed.v1` | Void failed (manual intervention) |
| `payment.refund_initiated.v1` | Refund requested |
| `payment.refund_completed.v1` | Refund settled |
| `payment.refund_failed.v1` | Refund failed |
| `payment.chargeback_received.v1` | Chargeback / dispute notification |
| `payment.webhook_signature_failed.v1` | Potential security event |

## Subscribed Events

| Event | Action |
|---|---|
| `platform.tenant.provisioned.v1` | Prepare per-tenant structures (no credentials yet) |
| `platform.tenant.deleted.v1` | Mark tenant as inactive; retain transactions for legal retention |
| `identity.user.deleted.v1` | No deletion — transactions retained per ADR-005 (financial record); replace associated `actor_id` on manual refund with pseudonym; publish `platform.gdpr.cascade_completed.v1` |

## Configuration

```yaml
yc-payment:
  listen: :8080
  database:
    url: postgres://yc_payment:...@...
  encryption:
    credentials_cipher_key_env: YC_PAYMENT_CIPHER_KEY
  gateways:
    stripe:
      api_base: https://api.stripe.com
      timeout_seconds: 30
    yoco:
      api_base: https://payments.yoco.com/api
      timeout_seconds: 30
  webhooks:
    stripe_path: /webhooks/stripe
    yoco_path: /webhooks/yoco
    signature_tolerance_seconds: 300
  nats:
    url: nats://...
  log_level: info
```

## Dependencies on Other Services

- **None internal at startup**
- External: Stripe API, Yoco API

## Called By

- `yc-checkout` (synchronous saga calls, ADR-019)
- `yc-orders` (refunds initiated post-order, `POST /internal/refund`)
- Shop admins (manual refunds)
- Gateways (webhooks)

## GDPR Behavior

- **No card data stored** (ADR-009)
- Transactions contain `tenant_id`, `order_id`, amounts — minimal PII
- Linkage to a user is via `order_id` → yc-orders (which handles pseudonymization)
- On `identity.user.deleted.v1`: no deletion of transactions (financial retention); `actor_id` on admin-initiated refunds pseudonymized
- Payment gateway tokens never persisted beyond the single API call
- Publish `platform.gdpr.cascade_completed.v1` so the yc-audit register can close the cascade

*Phase 8:* Implemented (ADR-025): refunds issued by the deleted shop admin get the pseudonym as `actor_id`; the action is reported as `pseudonymized`, with a count of 0 for customers.

## Security

- All gateway credentials encrypted at rest
- Webhook signature verification mandatory; failures publish security event
- Rate limiting on webhook endpoints
- Credentials never logged
- Audit log entry for every credential change (via yc-audit)

## Open Questions / Future Scope

- Saved payment methods (gateway-side only; store `customer_id` reference)
- Apple Pay / Google Pay flows
- 3D Secure 2 handling nuances
- Alternative payment methods (EFT, Ozow, PayFast)
- PayPal driver
- Marketplace / split payments (platform fee to tenant, remainder to supplier)
- Automated reconciliation reports
- Chargeback response workflow (upload evidence)
- Subscription billing
