# yc-shipping — Service Requirement

## Overview

`yc-shipping` manages shipping zones, rates, carrier integrations, and tracking. It provides quotes to yc-checkout, books shipments (labels and tracking numbers) when yc-orders packages an order, and tracks fulfillment through to delivery. Carrier integrations are implemented as drivers behind a common interface. The flow between checkout, orders and shipping is ADR-022.

## Location in Monorepo

```
/services/yc-shipping/
```

## Responsibilities

1. Maintain shipping zones (geographic groupings)
2. Maintain shipping methods and rates per zone
3. Quote shipping options to yc-checkout given a cart and delivery address
4. Book shipments (label, tracking number) when the shop admin packages an order
5. Track shipments via carrier APIs or webhooks
6. Emit tracking lifecycle events
7. Handle delivery exceptions (failed delivery, address issues)
8. Skip entirely for all-digital orders (per ADR-010 waiver)
9. Optional delivery schedules and capacity-backed time slots for rates that require them (issue #30, ADR-022 amendment)

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
  "zone_id": "…",
  "name": "Gauteng",
  "priority": 10,
  "match_rules": [
    { "country_code": "ZA", "fields": { "province": "Gauteng" } }
  ]
}
```

Address matching:
- Uses `country_code` from the delivery address (ADR-015)
- Optional `fields` values must equal the address's `fields` values (case-insensitive, trimmed)
- Zones are tried by `priority` (lowest first, then name); the first zone with a matching rule wins

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
  "carrier_driver": "manual",
  "schedule_id": null,
  "requires_delivery_slot": false
}
```

Rates are **in store currency** (ADR-014). When `requires_delivery_slot` is true, `schedule_id` must reference an existing delivery schedule (hard 400 otherwise). Quote options expose both fields so checkout can require a slot.

## Delivery schedules and slots (WP4c / #30)

Shop admins configure **schedules** (timezone, weekdays, open/close, window length, capacity, lead time, same-day cut-off, blackouts). Windows are generated; capacity is tracked in `slot_bookings`. Default timezone is `Africa/Johannesburg`. Weekdays use Go's `time.Weekday` (`0`=Sunday … `6`=Saturday). Slot IDs are deterministic: `{schedule_id}_{unix_start_utc}`.

Checkout lists soft-available windows, verifies capacity on select (`GET /internal/slots/{slot_id}`), and **hard-books at ORDER_CREATE** (`POST …/book` with `booking_key` = checkout session ID). Abandon/compensate calls `POST …/release`. Admin reschedule after place is out of scope.

## Quote Flow

```
yc-checkout → yc-shipping: POST /internal/quote   (checkout's shipping step, ADR-022 rule 1)
  Request: delivery_address, items (sku_id, quantity, weight_grams, is_digital), subtotal_minor
  yc-shipping:
    1. Resolve zone from address
    2. If no zone matches: return empty options (yc-checkout answers 409)
    3. For each active rate in the zone: compute the amount; skip a weight-tiered
       rate when a physical item has no weight or the parcel is over the last tier
    4. Return the options, cheapest first
```

## Fulfillment Flow

```
yc-orders → yc-shipping: POST /internal/shipments   (admin packages the order, ADR-022 rule 4)
  Request: order_id, order_number, method, rate_id, delivery_address; Idempotency-Key
  1. Return the order's shipment if it already has one
  2. Resolve the rate's carrier driver (manual if the rate is gone)
  3. Call driver.CreateLabel → tracking number (and label URL)
  4. Store the shipment as label_created; publish shipping.label_created.v1
  5. Status updates (admin manual-update now; carrier webhooks or polling later):
     publish shipping.in_transit.v1 / delivered.v1 / delivery_exception.v1 (yc-orders consumes)
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
| GET | `/admin/shipping/schedules` | List delivery schedules | shop_admin |
| POST | `/admin/shipping/schedules` | Create schedule | shop_admin |
| GET | `/admin/shipping/schedules/{id}` | Get schedule | shop_admin |
| PUT | `/admin/shipping/schedules/{id}` | Replace schedule | shop_admin |
| DELETE | `/admin/shipping/schedules/{id}` | Delete schedule (409 if rates still reference) | shop_admin |
| GET | `/admin/shipments` | List shipments | shop_admin |
| GET | `/admin/shipments/{id}` | Shipment detail | shop_admin |
| POST | `/admin/shipments/{id}/manual-update` | Update tracking manually (for `manual` driver) | shop_admin |
| GET | `/admin/carriers` | List carrier configs (deferred until a carrier driver exists) | shop_admin |
| PUT | `/admin/carriers/{driver}` | Configure carrier credentials (deferred) | shop_admin |

### Internal Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| POST | `/internal/quote` | Shipping quote for checkout | service |
| GET | `/internal/slots` | List available slots for a rate (`rate_id`, `from`, `to`; max 14 days) | service |
| GET | `/internal/slots/{slot_id}` | Soft capacity check for one slot (404 if full/invalid) | service |
| POST | `/internal/slots/{slot_id}/book` | Hard book at ORDER_CREATE (`booking_key`) | service |
| POST | `/internal/slots/{slot_id}/release` | Release on abandon/compensate | service |
| POST | `/internal/shipments` | Book the order's shipment (yc-orders, on packaging) | service |

### Webhook Endpoints

| Method | Path | Description |
|---|---|---|
| POST | `/webhooks/{driver}` | Carrier webhook (future) |

### Phase 6 Implementation Notes

The contract is `services/yc-shipping/api/openapi.yaml`; the flow is ADR-022 (including the 2026-10-28 delivery-slots amendment). Implemented: zones, rates, delivery schedules and internal slot list/get/book/release, the quote (with `requires_delivery_slot` / `schedule_id` on options), shipment booking with the `manual` driver, admin shipment list, detail and manual updates, public zones and tracking, and cancellation on `order.cancelled.v1`. Checkout binds slots via `GET`/`PUT /checkout/{id}/slots|slot` and books/releases through these internal APIs (issue #30). Carrier configuration, carrier webhooks and the GDPR and tenant subscriptions come with the first real carrier driver.

- **Manual driver.** Tracking numbers are `YC` followed by 12 random characters of Crockford's base 32 (digits and capitals without I, L, O or U); there is no label URL. The admin reports `in_transit` (with an optional carrier name and reference), `delivered` and `exception` (a message is required). `in_transit` and `exception` may repeat; a shipment moves label_created → in_transit → delivered, with `exception` from any of the first two and back.
- **Booking** is idempotent per order (`UNIQUE (tenant_id, order_id)`) as well as by `Idempotency-Key`. A booking for a rate that has been deleted uses the method name and the manual driver.
- **Rates** must have sensible tiers: weight tiers with increasing `max_weight_g`, price tiers starting at 0 with increasing `min_subtotal_minor`, amounts not negative, and `estimated_days_min` ≤ `estimated_days_max`. A rate can be made inactive instead of deleted. Deleting a zone deletes its rates.
- **Public tracking** returns the status, method, carrier and events only, never the address or order.
- **Cancellation** applies to shipments still `label_created`; a dispatched shipment is left to the shop admin.

### Phase 8 Implementation Notes

- **GDPR:** yc-shipping subscribes to `identity.user.deleted.v1` and calls yc-orders `GET /internal/customer-orders` (ADR-021) as client `yc-shipping` (`internal_calls`, `YC_SHIPPING_SERVICE_SECRET`); see GDPR Behavior.

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

-- Weekdays: 0=Sunday … 6=Saturday. Windows generated; capacity in slot_bookings.
CREATE TABLE delivery_schedules (
    id                  UUID PRIMARY KEY,
    tenant_id           TEXT NOT NULL,
    name                TEXT NOT NULL,
    timezone            TEXT NOT NULL DEFAULT 'Africa/Johannesburg',
    window_minutes      INT NOT NULL,
    weekdays            INT[] NOT NULL,
    open_time           TIME NOT NULL,
    close_time          TIME NOT NULL,
    capacity_per_window INT NOT NULL,
    min_lead_minutes    INT NOT NULL DEFAULT 0,
    same_day_cutoff     TIME,
    blackout_dates      DATE[] NOT NULL DEFAULT '{}',
    active              BOOLEAN NOT NULL DEFAULT TRUE,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE slot_bookings (
    id          UUID PRIMARY KEY,
    tenant_id   TEXT NOT NULL,
    schedule_id UUID NOT NULL REFERENCES delivery_schedules(id) ON DELETE CASCADE,
    slot_start  TIMESTAMPTZ NOT NULL,
    slot_end    TIMESTAMPTZ NOT NULL,
    booking_key TEXT NOT NULL,          -- checkout_session_id
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, schedule_id, slot_start, booking_key)
);

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
    schedule_id          UUID REFERENCES delivery_schedules(id) ON DELETE RESTRICT,
    requires_delivery_slot BOOLEAN NOT NULL DEFAULT FALSE,
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
| `shipping.label_created.v1` | Shipment booked: tracking number (and label) created |
| `shipping.in_transit.v1` | Shipment is moving — yc-orders moves the order to `shipped` |
| `shipping.delivered.v1` | Delivered — yc-orders moves the order to `delivered` |
| `shipping.delivery_exception.v1` | Problem (failed delivery, address issue) — order `delivery_exception` |
| `shipping.cancelled.v1` | Shipment cancelled (order cancelled before dispatch) |

Future, with carrier drivers: `shipping.rate_quoted.v1` (telemetry), `shipping.label_failed.v1`, `shipping.carrier_pickup.v1`, `shipping.out_for_delivery.v1`.

## Subscribed Events

| Event | Action |
|---|---|
| `order.cancelled.v1` | Cancel the order's shipment if not yet dispatched (ADR-022 rule 6) |
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

- None at runtime: yc-checkout sends SKU weights with the quote (from yc-product), and yc-orders sends the delivery address with the booking
- External: carrier APIs (per driver)

## Called By

- `yc-checkout` (quotes)
- `yc-orders` (booking on packaging; consumes shipping events)
- Clients (tracking lookup)
- Shop admins

## GDPR Behavior

- `delivery_address` is PII
- Retained for the shipment retention window (2 years per ADR-004)
- On `identity.user.deleted.v1`: pseudonymize address on shipments outside the operational window; retain inside the window for carrier reconciliation
- Publish `platform.gdpr.cascade_completed.v1`

*Phase 8:* Implemented: shipments do not record the customer, so yc-shipping asks yc-orders for the customer's orders (by user ID and pseudonym, with its new service secret) and clears their delivery addresses at once; the action is reported as `anonymized`.

## Open Questions / Future Scope

- Real carrier integrations (Aramex, DHL, PostNet, PUDO, Courier Guy)
- Multi-package shipments
- International customs documentation
- Returns / RMA shipping (returns are refunded without a return shipment in v1, ADR-022 rule 7)
- Rates by dimensions (SKU dimensions are stored by yc-product but unused)
- Live rate shopping (query multiple carriers, pick cheapest)
- Shipping insurance options
- Dim weight calculation
- Pickup point delivery (locker, store)
- Admin reschedule of a booked delivery slot after place (slot MVP books at ORDER_CREATE only)
