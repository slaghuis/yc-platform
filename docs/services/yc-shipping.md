# yc-shipping — Service Requirement

## Overview

`yc-shipping` manages shipping zones, rates, carrier integrations, and tracking. It provides quotes to yc-checkout, generates shipping labels on order placement, and tracks fulfillment through to delivery. Carrier integrations are implemented as drivers behind a common interface.

## Location in Monorepo

```
/services/yc-shipping/
```

## Responsibilities

1. Maintain shipping zones (geographic groupings)
2. Maintain shipping methods and rates per zone
3. Quote shipping options to yc-checkout given a cart and delivery address
4. Generate shipping labels when orders are placed
5. Track shipments via carrier APIs or webhooks
6. Emit tracking lifecycle events
7. Handle delivery exceptions (failed delivery, address issues)
8. Skip entirely for all-digital orders (per ADR-010 waiver)

## Multi-Tenancy

- Zones, rates, and carrier config are all tenant-scoped
- Tenants may enable zero or more carrier drivers

## Carrier Driver Interface

```go
type CarrierDriver interface {
    Name() string
    Quote(ctx, QuoteRequest)       (QuoteResponse, error)
    CreateLabel(ctx, LabelRequest) (LabelResponse, error)
    Track(ctx, TrackRequest)       (TrackResponse, error)
    Cancel(ctx, CancelRequest)     (CancelResponse, error)
    ParseWebhook(ctx, WebhookRequest) (WebhookEvent, error)
}
```

### v1 Drivers

| Driver | Scope | Notes |
|---|---|---|
| `manual` | Any | No external integration — admin manually fulfills; tracking numbers entered manually |
| `pudo-aramex` or similar SA courier | South Africa | Future — leaves hook; v1 may ship with `manual` only |

The `manual` driver is sufficient for v1 to make the saga complete end-to-end. Specific carrier integrations are deferred and defined as new drivers.

## Zone Definition

A zone is an arbitrary grouping of regions. Tenants define them.

```json
{
  "zone_id": "za-gauteng",
  "name": "Gauteng",
  "match_rules": [
    { "country_code": "ZA", "region": "Gauteng" }
  ]
}
```

Address matching:
- Uses `country_code` from the delivery address (ADR-015)
- Optional additional fields matched against the address's `fields` JSON
- First matching zone wins; evaluation order is tenant-configured

## Rate Definition

Per zone, per method:

```json
{
  "rate_id": "...",
  "zone_id": "za-gauteng",
  "method": "standard",
  "display_name": "Standard Delivery (2–3 days)",
  "pricing_type": "flat|weight_tiered|price_tiered",
  "flat_amount_minor": 7500,
  "weight_tiers": [
    { "max_weight_g": 1000, "amount_minor": 5000 },
    { "max_weight_g": 5000, "amount_minor": 7500 }
  ],
  "price_tiers": [
    { "min_subtotal_minor": 0, "amount_minor": 7500 },
    { "min_subtotal_minor": 50000, "amount_minor": 0 }
  ],
  "estimated_days_min": 2,
  "estimated_days_max": 3,
  "carrier_driver": "manual"
}
```

Rates are **in store currency** (ADR-014).

## Quote Flow

```
yc-checkout → yc-shipping: POST /internal/quote
  Request: cart items (with weights/dimensions from yc-product), delivery_address
  yc-shipping:
    1. Resolve zone from address
    2. If no zone matches: return empty options (yc-checkout fails confirm)
    3. For each enabled method in the zone: compute rate
    4. Return list of options
```

## Fulfillment Flow

```
yc-orders → yc-shipping (via order.placed.v1):
  1. Fetch order details
  2. If order.requires_shipping = false: no-op
  3. Resolve the chosen rate's carrier driver
  4. Call driver.CreateLabel
  5. Store label and tracking number
  6. Publish shipping.label_created.v1 (yc-orders consumes)
  7. On carrier webhook or polling: update status, publish tracking events
```

## Delivery Exceptions

- Carrier reports a failed delivery or address issue
- yc-shipping publishes `shipping.delivery_exception.v1`
- yc-orders flags the order; yc-message notifies the customer
- Resolution: shop admin coordinates manually in v1

## API

### Public Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| GET | `/shipping/zones` | Public zone list (for frontend display) | any |
| GET | `/shipping/tracking/{tracking_number}` | Public tracking lookup | any |

### Shop Admin Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| GET | `/admin/zones` | List zones | shop_admin |
| POST | `/admin/zones` | Create zone | shop_admin |
| PUT | `/admin/zones/{id}` | Update zone | shop_admin |
| DELETE | `/admin/zones/{id}` | Delete zone | shop_admin |
| GET | `/admin/rates` | List rates | shop_admin |
| POST | `/admin/rates` | Create rate | shop_admin |
| PUT | `/admin/rates/{id}` | Update rate | shop_admin |
| DELETE | `/admin/rates/{id}` | Delete rate | shop_admin |
| GET | `/admin/shipments` | List shipments | shop_admin |
| GET | `/admin/shipments/{id}` | Shipment detail | shop_admin |
| POST | `/admin/shipments/{id}/manual-update` | Update tracking manually (for `manual` driver) | shop_admin |
| GET | `/admin/carriers` | List carrier configs | shop_admin |
| PUT | `/admin/carriers/{driver}` | Configure carrier credentials | shop_admin |

### Internal Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| POST | `/internal/quote` | Shipping quote for checkout | service |
| POST | `/internal/shipments` | Create shipment (post-order) | service |

### Webhook Endpoints

| Method | Path | Description |
|---|---|---|
| POST | `/webhooks/{driver}` | Carrier webhook (future) |

## Database Schema

Database: **`yc_shipping`**

```sql
CREATE TABLE zones (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       TEXT NOT NULL,
    name            TEXT NOT NULL,
    match_rules     JSONB NOT NULL,         -- array of rule objects
    priority        INT NOT NULL DEFAULT 100,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_zones_tenant ON zones (tenant_id, priority);

CREATE TABLE rates (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            TEXT NOT NULL,
    zone_id              UUID NOT NULL REFERENCES zones(id) ON DELETE CASCADE,
    method               TEXT NOT NULL,
    display_name         TEXT NOT NULL,
    pricing_type         TEXT NOT NULL,     -- 'flat','weight_tiered','price_tiered'
    flat_amount_minor    BIGINT,
    weight_tiers         JSONB,
    price_tiers          JSONB,
    estimated_days_min   INT,
    estimated_days_max   INT,
    carrier_driver       TEXT NOT NULL,
    status               TEXT NOT NULL DEFAULT 'active',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_rates_zone ON rates (zone_id, status);

CREATE TABLE carrier_configs (
    tenant_id       TEXT NOT NULL,
    driver          TEXT NOT NULL,
    enabled         BOOLEAN NOT NULL DEFAULT FALSE,
    credentials_enc BYTEA,
    config          JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, driver)
);

CREATE TABLE shipments (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       TEXT NOT NULL,
    order_id        UUID NOT NULL,
    rate_id         UUID,
    carrier_driver  TEXT NOT NULL,
    tracking_number TEXT,
    label_url       TEXT,
    status          TEXT NOT NULL,          -- 'pending','label_created','in_transit','delivered','exception','cancelled'
    delivery_address JSONB NOT NULL,
    carrier_metadata JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_shipments_order ON shipments (order_id);
CREATE INDEX idx_shipments_tracking ON shipments (tracking_number);

CREATE TABLE shipment_events (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    shipment_id     UUID NOT NULL REFERENCES shipments(id) ON DELETE CASCADE,
    event_type      TEXT NOT NULL,
    message         TEXT,
    occurred_at     TIMESTAMPTZ NOT NULL,
    recorded_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE outbox ( ... );
CREATE TABLE processed_events ( ... );

ALTER TABLE shipments ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON shipments
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform');
```

## Published Events

| Event | When |
|---|---|
| `shipping.rate_quoted.v1` | Quote returned to checkout (optional telemetry) |
| `shipping.label_created.v1` | Label created |
| `shipping.label_failed.v1` | Label creation failed |
| `shipping.carrier_pickup.v1` | Carrier collected the package |
| `shipping.in_transit.v1` | Shipment is moving |
| `shipping.out_for_delivery.v1` | Final-mile started |
| `shipping.delivered.v1` | Delivered — triggers yc-orders transition |
| `shipping.delivery_exception.v1` | Problem (failed delivery, address issue) |
| `shipping.cancelled.v1` | Shipment cancelled |

## Subscribed Events

| Event | Action |
|---|---|
| `order.placed.v1` | If `requires_shipping`: create shipment, call carrier driver |
| `order.cancelled.v1` | Cancel shipment if not yet dispatched |
| `platform.tenant.provisioned.v1` | Create empty tenant structures |
| `platform.tenant.deleted.v1` | Delete tenant zones, rates; retain shipments for retention window |
| `identity.user.deleted.v1` | Pseudonymize `delivery_address` on completed shipments beyond retention; else retain for operational integrity |

## Configuration

```yaml
yc-shipping:
  listen: :8080
  database:
    url: postgres://yc_shipping:...@...
  encryption:
    credentials_cipher_key_env: YC_SHIPPING_CIPHER_KEY
  drivers:
    manual:
      enabled: true
    # carrier drivers added here
  tracking_poll_interval_minutes: 60   # for non-webhook carriers
  nats:
    url: nats://...
  log_level: info
```

## Dependencies on Other Services

- **yc-product** (for SKU dimensions/weights at quote time — optional, cached locally)
- External: carrier APIs (per driver)

## Called By

- `yc-checkout` (quotes)
- `yc-orders` (via events)
- Clients (tracking lookup)
- Shop admins

## GDPR Behavior

- `delivery_address` is PII
- Retained for the shipment retention window (2 years per ADR-004)
- On `identity.user.deleted.v1`: pseudonymize address on shipments outside the operational window; retain inside the window for carrier reconciliation
- Publish `platform.gdpr.cascade_completed.v1`

## Open Questions / Future Scope

- Real carrier integrations (Aramex, DHL, PostNet, PUDO, Courier Guy)
- Multi-package shipments
- International customs documentation
- Returns / RMA shipping
- Delivery scheduling / time slots
- Live rate shopping (query multiple carriers, pick cheapest)
- Shipping insurance options
- Dim weight calculation
- Pickup point delivery (locker, store)
