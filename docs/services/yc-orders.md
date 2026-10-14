# yc-orders — Service Requirement

## Overview

`yc-orders` is the authoritative source for orders. yc-checkout creates an order as `pending_payment` at the `ORDER_CREATE` step and confirms it once payment has succeeded (ADR-019); only then is the order placed. Orders have a lifecycle (received → packaged → shipped → delivered) with transitions driven by yc-shipping events and shop admin actions. Refunds, cancellations, and returns are managed here.

## Location in Monorepo

```
/services/yc-orders/
```

## Responsibilities

1. Create pending orders for yc-checkout, then confirm or abandon them (synchronous internal calls, ADR-019)
2. Store the full order snapshot (items, pricing, taxes, addresses, customer references)
3. Maintain order status through the lifecycle
4. Allow shop admin status updates (packaged, note additions)
5. Receive shipping status updates and advance order state
6. Handle cancellations (triggers refund via yc-payment, release of inventory if not yet shipped)
7. Handle refunds (initiated by shop admin or customer-initiated requests)
8. Expose order history to customers and management views to shop admins
9. Provide verified-purchase lookups to yc-review
10. Participate in GDPR cascade with pseudonymization (financial retention)

## Multi-Tenancy

- All orders are tenant-scoped
- Shop admins see only their tenant's orders

## Order Lifecycle

```
pending_payment ─▶ abandoned            (checkout compensation; terminal)
       │
       ▼
received ─▶ packaged ─▶ shipped ─▶ delivered ─▶ (completed)
    │           │           │
    │           │           └─▶ delivery_exception ─▶ (resolution path)
    │           │
    └─▶ cancelled (refund issued if captured)
    
                                 └─▶ returned ─▶ refunded (partial or full)

disputed (triggered by chargeback) ─▶ resolved (won/lost)
```

- `pending_payment` = created by yc-checkout, payment not yet captured; no events published, hidden from customer and shop admin listings
- `abandoned` = checkout compensated before payment succeeded; hidden, no events published
- `received` = paid and confirmed by yc-checkout; `order.placed.v1` published
- `packaged` = shop admin marks ready for carrier
- `shipped` = yc-shipping event received
- `delivered` = yc-shipping event received
- `cancelled` = pre-shipment cancellation
- `returned` = post-delivery return accepted
- `disputed` = chargeback received

Terminal states: `abandoned`, `delivered`, `cancelled`, `refunded`, `returned`.

## Order Snapshot

Orders capture a point-in-time snapshot. The order record **does not reference live catalog data** — product names, prices, and tax rates are stored on the order so historical accuracy is guaranteed even if the catalog changes.

```json
{
  "order_id": "...",
  "tenant_id": "...",
  "customer": {
    "user_id": "...",            // may be pseudonymized after GDPR
    "user_type": "customer|visitor"
  },
  "guest_contact": {              // for visitor orders
    "email": "...",
    "phone_e164": "..."
  },
  "delivery_address": { ... },    // free-form JSON (ADR-015)
  "requires_shipping": true,
  "items": [
    {
      "sku_id": "...",
      "sku_code": "...",
      "name_snapshot": "...",
      "variant_values_snapshot": {...},
      "quantity": 2,
      "unit_price_minor": 10000,
      "line_total_minor": 20000,
      "tax_breakdown": [...]
    }
  ],
  "pricing_snapshot": { ... },    // full PricingResult
  "shipping_option_snapshot": {...},
  "payment": {
    "transaction_id": "...",
    "gateway": "yoco",
    "amount_minor": 20000,
    "currency": "ZAR"
  },
  "status": "received",
  "status_history": [...],
  "notes_internal": [],            // shop admin notes
  "notes_customer": []              // customer-visible notes
}
```

## Verified Purchase Check (for yc-review)

Internal endpoint:
```
GET /internal/orders/user/{user_id}/has_purchased_product/{product_id}
Response: { "purchased": true, "last_delivered_at": "..." }
```

Logic: query orders where `user_id` matches, `product_id` appears in `items`, and status reached `delivered`.

## Guest Order Registration Hook

When a visitor's order is complete and the visitor later registers with the same email:
- yc-identity emits `identity.user.registered.v1`
- yc-orders listens, looks for guest orders matching the email, and offers to **associate** them with the new customer (not automatic to prevent email spoofing)
- The frontend presents the opportunity post-registration; customer confirms
- yc-orders then updates `user_id` to the new customer

## API

### Customer Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| GET | `/orders/mine` | List my orders | customer |
| GET | `/orders/{id}` | Get order detail | customer (owner) |
| POST | `/orders/{id}/cancel-request` | Request cancellation (pre-shipment) | customer (owner) |
| POST | `/orders/{id}/return-request` | Request return (post-delivery) | customer (owner) |
| POST | `/orders/claim-guest-orders` | Claim guest orders after registration | customer |

### Shop Admin Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| GET | `/admin/orders` | List orders (filterable) | shop_admin |
| GET | `/admin/orders/{id}` | Order detail | shop_admin |
| POST | `/admin/orders/{id}/package` | Mark as packaged | shop_admin |
| POST | `/admin/orders/{id}/cancel` | Cancel order (triggers refund if captured) | shop_admin |
| POST | `/admin/orders/{id}/refund` | Issue refund (full or partial) | shop_admin |
| POST | `/admin/orders/{id}/return/accept` | Accept return | shop_admin |
| POST | `/admin/orders/{id}/notes/internal` | Add internal note | shop_admin |
| POST | `/admin/orders/{id}/notes/customer` | Add customer-visible note | shop_admin |

### Internal Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| POST | `/internal/orders` | Create a `pending_payment` order (yc-checkout, ORDER_CREATE) | service |
| POST | `/internal/orders/{id}/confirm` | Payment succeeded: → `received`, publish `order.placed.v1` (yc-checkout, CONFIRMING) | service |
| POST | `/internal/orders/{id}/abandon` | Checkout compensation: `pending_payment` → `abandoned` | service |
| GET | `/internal/orders/{id}` | Fetch order | service |
| GET | `/internal/orders/user/{user_id}/has_purchased_product/{product_id}` | Verified purchase | service |

The create, confirm and abandon endpoints require `Idempotency-Key` and return the stored response on a repeated key. Confirm and abandon are idempotent on state: confirming a `received` order or abandoning an `abandoned` (or never-created) order succeeds without side effects.

## Database Schema

Database: **`yc_orders`**

```sql
CREATE TABLE orders (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          TEXT NOT NULL,
    order_number       TEXT NOT NULL,          -- human-readable, e.g., "SHOP-00042"
    user_id            TEXT NOT NULL,          -- pseudonymized post-GDPR
    user_type          TEXT NOT NULL,
    guest_contact      JSONB,                   -- for visitor orders; nullified post-retention
    delivery_address   JSONB,
    requires_shipping  BOOLEAN NOT NULL,
    status             TEXT NOT NULL,
    items              JSONB NOT NULL,          -- snapshot array
    pricing_snapshot   JSONB NOT NULL,
    shipping_snapshot  JSONB,
    payment_transaction_id UUID,
    amount_minor       BIGINT NOT NULL,
    currency           TEXT NOT NULL,
    checkout_session_id UUID,
    source_email       TEXT,                    -- for guest order association by email
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, order_number)
);
CREATE INDEX idx_orders_tenant_user ON orders (tenant_id, user_id);
CREATE INDEX idx_orders_tenant_status ON orders (tenant_id, status);
CREATE INDEX idx_orders_source_email ON orders (tenant_id, source_email)
    WHERE user_type = 'visitor';

CREATE TABLE order_status_history (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id        UUID NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    from_status     TEXT,
    to_status       TEXT NOT NULL,
    actor_id        TEXT,
    reason          TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE order_notes (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id        UUID NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    visibility      TEXT NOT NULL,              -- 'internal','customer'
    body            TEXT NOT NULL,
    author_id       TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE return_requests (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id        UUID NOT NULL REFERENCES orders(id),
    items           JSONB NOT NULL,
    reason          TEXT,
    status          TEXT NOT NULL,              -- 'pending','accepted','rejected','received','refunded'
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at     TIMESTAMPTZ
);

CREATE SEQUENCE order_number_seq;

CREATE TABLE outbox ( ... );
CREATE TABLE processed_events ( ... );

ALTER TABLE orders ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON orders
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform');
```

## Published Events

| Event | When |
|---|---|
| `order.placed.v1` | Order confirmed after payment (`POST /internal/orders/{id}/confirm`) |
| `order.packaged.v1` | Shop admin marked ready |
| `order.shipped.v1` | Shipping confirmed |
| `order.delivered.v1` | Delivery confirmed |
| `order.cancelled.v1` | Order cancelled |
| `order.cancellation_failed.v1` | Cancellation blocked (already shipped) |
| `order.refund_requested.v1` | Refund initiated |
| `order.refunded.v1` | Refund completed |
| `order.return_requested.v1` | Customer requested return |
| `order.return_accepted.v1` | Shop admin accepted return |
| `order.return_received.v1` | Returned items received |
| `order.disputed.v1` | Chargeback flagged |
| `order.note_added.v1` | Note added (internal or customer-visible) |

## Subscribed Events

| Event | Action |
|---|---|
| `shipping.label_created.v1` | Update order with tracking info |
| `shipping.in_transit.v1` | Advance to `shipped` |
| `shipping.delivered.v1` | Advance to `delivered` |
| `shipping.delivery_exception.v1` | Flag exception |
| `payment.refund_completed.v1` | Mark order (or portion) refunded |
| `payment.chargeback_received.v1` | Transition to `disputed` |
| `identity.user.registered.v1` | Check for guest orders by email; expose claim opportunity |
| `identity.user.deleted.v1` | Pseudonymize `user_id`, clear `guest_contact.phone`, keep order record (financial); publish `platform.gdpr.cascade_completed.v1` |
| `platform.tenant.provisioned.v1` | Create per-tenant order number sequence (if needed) |
| `platform.tenant.deleted.v1` | Mark tenant inactive; retain orders for legal retention |

## Configuration

```yaml
yc-orders:
  listen: :8080
  database:
    url: postgres://yc_orders:...@...
  order_number:
    format: "{tenant_prefix}-{seq:06d}"
    sequence_strategy: per_tenant
  cancellation:
    allow_after_shipment: false
  nats:
    url: nats://...
  log_level: info
```

## Dependencies on Other Services

- **yc-payment** (refunds)
- **yc-shipping** (status updates via events)
- **yc-inventory** (release reservation on cancel pre-shipment via events)

## Called By

- Customers (view/cancel/return)
- Shop admins (fulfillment)
- `yc-checkout` (create, confirm, abandon — ADR-019)
- `yc-review` (verified purchase check)

## GDPR Behavior

- Orders are **financial records** — retained for 7 years per ADR-004
- On `identity.user.deleted.v1`:
  - Replace `user_id` with pseudonym from yc-identity
  - After the operational retention window (configurable, default 90 days), nullify `guest_contact.phone`, retain email hash only
  - Retain items, amounts, timestamps for accounting
- Publish `platform.gdpr.cascade_completed.v1`

## Open Questions / Future Scope

- Partial fulfillment (ship items as they become available)
- Split orders by supplier
- Order editing (change address, quantity) post-placement
- Gift messages and gift wrapping
- Subscription order renewals
- Marketplace commission calculation
- Return merchandise authorization (RMA) workflow
- Automated return label generation
- Store credit as refund option
- Multi-currency reporting (reporting dashboard aggregates in a reporting currency)
