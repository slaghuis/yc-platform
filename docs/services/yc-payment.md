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

When Yoco is chosen, yc-checkout uses the Variant B flow (charge instead of auth+capture). Compensation of a failed order creation uses **refund**, not void.

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
    priority           INT NOT NULL DEFAULT 100,
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
| `payment.authorization_requested.v1` | Perform authorization |
| `payment.capture_requested.v1` | Perform capture |
| `payment.charge_requested.v1` | Perform combined charge |
| `payment.void_requested.v1` | Perform void |
| `payment.refund_requested.v1` | Perform refund |
| `platform.tenant.provisioned.v1` | Prepare per-tenant structures (no credentials yet) |
| `platform.tenant.deleted.v1` | Mark tenant as inactive; retain transactions for legal retention |
| `identity.user.deleted.v1` | No deletion — transactions retained per ADR-005 (financial record); replace associated `actor_id` on manual refund with pseudonym |

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

- `yc-checkout` (saga command events)
- `yc-orders` (refunds initiated post-order)
- Shop admins (manual refunds)
- Gateways (webhooks)

## GDPR Behavior

- **No card data stored** (ADR-009)
- Transactions contain `tenant_id`, `order_id`, amounts — minimal PII
- Linkage to a user is via `order_id` → yc-orders (which handles pseudonymization)
- On `identity.user.deleted.v1`: no deletion of transactions (financial retention); `actor_id` on admin-initiated refunds pseudonymized
- Payment gateway tokens never persisted beyond the single API call

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
