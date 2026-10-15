# yc-inventory — Service Requirement

## Overview

`yc-inventory` is the authoritative source for stock levels. It tracks per-SKU stock, handles reservations during checkout with TTL-based expiry, commits stock on order placement, and releases stock on compensation. Only SKUs flagged with `inventory_tracked = true` are managed; the service respects tenant-level and SKU-level opt-outs (ADR-012).

## Location in Monorepo

```
/services/yc-inventory/
```

## Responsibilities

1. Track stock levels per SKU per tenant
2. Accept stock-received events from admins (receiving, restocking)
3. Allow admin stock adjustments (shrinkage, corrections)
4. Reserve stock atomically for checkout sessions with TTL
5. Expire reservations automatically when TTL elapses
6. Commit reservations on order placement
7. Release reservations on checkout failure or cancellation
8. Emit low-stock warnings and out-of-stock events
9. Honor tenant-level `inventory.enabled` and SKU-level `inventory_tracked` flags
10. Support back-in-stock subscriptions (customer opts in; notified when restocked)

## Multi-Tenancy

- Stock levels are tenant-scoped
- A SKU exists only within one tenant's catalog (ADR confirmed in Part 2)
- Reservations are tenant-scoped and tied to a checkout session

## Stock Model

```
StockLevel (per tenant, per SKU)
  ├── on_hand         # physical count
  ├── reserved        # sum of active reservations
  ├── available       # on_hand - reserved (computed)
  ├── low_threshold   # trigger for low-stock event
  └── (no safety stock buffer in v1)
```

- All counts are **non-negative integers**
- The service rejects any operation that would make `on_hand` or `reserved` negative
- `available` is derived, never stored directly

## Reservation Lifecycle

```
            ┌──────────┐  TTL expires   ┌──────────┐
            │ reserved │ ─────────────▶ │ expired  │
            └────┬─────┘                └──────────┘
                 │
                 ├── committed (order placed)       ─▶ decrements on_hand
                 ├── released (checkout failed)     ─▶ returns to available
                 └── released (admin manual)        ─▶ returns to available
```

- **Reservation TTL default**: 15 minutes (shorter than checkout session TTL of 30 min, so stock doesn't lock longer than the saga)
- TTL is set by the reservation request (passed by yc-checkout) within a configured min/max window
- Expired reservations are swept by a background job (every 30 seconds)
- Expiry emits `inventory.stock.reservation_expired.v1` — yc-checkout listens and compensates if needed

## Opt-Out Behavior (ADR-012)

- If tenant has `inventory.enabled = false`: this service **has no stock rows for that tenant**. All reservation requests return success immediately with a null reservation.
- If a specific SKU has `inventory_tracked = false`: this service **has no stock row for that SKU**. Reservation requests that include this SKU treat it as infinite stock.
- yc-checkout should pre-filter tracked SKUs (per Part 2 saga definition), but yc-inventory must also be tolerant of untracked SKUs appearing in requests.

## API

### Public Endpoints

None directly for customers; stock is exposed indirectly via search (yc-product's `in_stock` facet).

### Shop Admin Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| GET | `/admin/stock` | List stock levels (paginated, filterable) | shop_admin |
| GET | `/admin/stock/{sku_id}` | Get stock level for one SKU | shop_admin |
| POST | `/admin/stock/{sku_id}/receive` | Receive stock (increment on_hand) | shop_admin |
| POST | `/admin/stock/{sku_id}/adjust` | Adjust on_hand (shrinkage, correction) with reason | shop_admin |
| PUT | `/admin/stock/{sku_id}/low-threshold` | Set low-stock threshold | shop_admin |
| GET | `/admin/stock/{sku_id}/reservations` | List active reservations for a SKU | shop_admin |
| POST | `/admin/reservations/{id}/release` | Manually release a reservation | shop_admin |
| GET | `/admin/stock/history/{sku_id}` | Movement history | shop_admin |

### Internal Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| POST | `/internal/reservations` | Reserve stock (yc-checkout, RESERVING) | service |
| POST | `/internal/reservations/{id}/commit` | Commit a reservation (yc-checkout, CONFIRMING — after payment) | service |
| POST | `/internal/reservations/{id}/release` | Release a reservation (yc-checkout compensation) | service |
| GET | `/internal/stock/batch` | Bulk stock lookup (used by yc-product for projection sync) | service |
| POST | `/internal/back-in-stock/subscribe` | Customer requests notification | service |

### Phase 4 Implementation Notes

The contract is `services/yc-inventory/api/openapi.yaml`. Implemented: the shop admin endpoints, the three reservation endpoints, the expiry sweeper and the stock events. Not yet: `/internal/stock/batch` (yc-product keeps `in_stock` from the stock events instead), back-in-stock subscriptions, and the tenant configuration-changed and tenant-deleted subscriptions. yc-inventory holds no customer data, so it takes no part in the GDPR cascade until back-in-stock subscriptions exist (ADR-025 rule 3).

- **Shop settings** are `GET`/`PUT /admin/inventory/config`: `inventory_enabled` and the default, minimum and maximum reservation TTL (defaults 15, 5 and 30 minutes, from `reservation` in the config). A row is created on `platform.tenant.provisioned.v1`; until a shop changes them, the service defaults apply. Turning inventory off publishes `inventory.stock.restored.v1` for every out-of-stock SKU, and turning it on publishes `inventory.stock.depleted.v1` for them, so yc-product's `in_stock` follows.
- **Movement history** is `GET /admin/stock/{sku_id}/history` rather than `/admin/stock/history/{sku_id}`, so every per-SKU route shares the `/admin/stock/{sku_id}` prefix.
- **Stock rows** follow `inventory_tracked` on `product.sku.created.v1`, `product.sku.updated.v1` and `product.sku.deleted.v1`. A newly tracked SKU starts at zero and is announced as depleted. Untracking or deleting a SKU drops its row; active reservations keep their lines, and committing or releasing them skips the SKU.
- **Reservations.** A shortage is a `409` problem (`error-handling.md`) with code `insufficient_stock` and a `shortages` member, rather than the `{"error": …}` body shown below. When nothing in the request is tracked, the response has no `reservation_id`. `ttl_minutes` is clamped to the shop's window.
- **Idempotency** on the internal POSTs uses the shared `httpkit` Idempotency-Key middleware: responses are kept in `idempotency_keys` for 24 hours and replayed on a repeated key.

## Reservation Request

```json
POST /internal/reservations
Idempotency-Key: {session_id}:reserve
{
  "session_id": "...",
  "items": [
    { "sku_id": "...", "quantity": 2 }
  ],
  "ttl_minutes": 15
}
```

Response (success):
```json
{
  "reservation_id": "...",
  "expires_at": "2025-01-15T10:15:00Z",
  "items": [...]
}
```

Response (failure — partial availability):
```json
{
  "error": "insufficient_stock",
  "shortages": [
    { "sku_id": "...", "requested": 5, "available": 2 }
  ]
}
```

Per ADR-012: **any shortage fails the whole reservation**. The response identifies specific SKUs so the frontend can inform the user which items need adjustment.

Reserve, commit and release are synchronous calls from yc-checkout (ADR-019). All three require `Idempotency-Key` and return the stored response on a repeated key. Release of an already-released, expired or unknown reservation succeeds as a no-op. Commit of a reservation that expired meanwhile (payment succeeded close to the TTL) still commits: it decrements `on_hand`, and if that takes available stock below zero it records the oversell for the shop admin instead of failing.

## Database Schema

Database: **`yc_inventory`**

```sql
CREATE TABLE tenant_inventory_config (
    tenant_id            TEXT PRIMARY KEY,
    inventory_enabled    BOOLEAN NOT NULL DEFAULT TRUE,
    default_reservation_ttl_minutes INT NOT NULL DEFAULT 15,
    min_reservation_ttl_minutes     INT NOT NULL DEFAULT 5,
    max_reservation_ttl_minutes     INT NOT NULL DEFAULT 30,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE stock_levels (
    tenant_id       TEXT NOT NULL,
    sku_id          UUID NOT NULL,
    on_hand         BIGINT NOT NULL DEFAULT 0 CHECK (on_hand >= 0),
    reserved        BIGINT NOT NULL DEFAULT 0 CHECK (reserved >= 0),
    low_threshold   BIGINT NOT NULL DEFAULT 0,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, sku_id),
    CHECK (reserved <= on_hand)
);

CREATE TABLE stock_movements (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       TEXT NOT NULL,
    sku_id          UUID NOT NULL,
    movement_type   TEXT NOT NULL,         -- 'receive','adjust','reserve','release','commit','expire'
    quantity_delta  BIGINT NOT NULL,       -- signed
    reason          TEXT,
    actor_id        TEXT,
    reference       TEXT,                   -- reservation_id, order_id, etc.
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_stock_movements_sku ON stock_movements (tenant_id, sku_id, created_at);

CREATE TABLE reservations (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       TEXT NOT NULL,
    session_id      TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    status          TEXT NOT NULL,         -- 'active','committed','released','expired'
    expires_at      TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    settled_at      TIMESTAMPTZ,
    UNIQUE (tenant_id, idempotency_key)
);
CREATE INDEX idx_reservations_active ON reservations (status, expires_at)
    WHERE status = 'active';

CREATE TABLE reservation_items (
    reservation_id  UUID NOT NULL REFERENCES reservations(id) ON DELETE CASCADE,
    sku_id          UUID NOT NULL,
    quantity        BIGINT NOT NULL CHECK (quantity > 0),
    PRIMARY KEY (reservation_id, sku_id)
);

CREATE TABLE back_in_stock_subscriptions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       TEXT NOT NULL,
    user_id         TEXT NOT NULL,
    sku_id          UUID NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    notified_at     TIMESTAMPTZ,
    UNIQUE (tenant_id, user_id, sku_id)
);

CREATE TABLE outbox ( ... );
CREATE TABLE processed_events ( ... );

ALTER TABLE stock_levels ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON stock_levels
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform');
-- similar on all tenant-scoped tables
```

## Concurrency Control

- Reservations must be **atomic**: either all items reserved or none
- Use a single transaction with `SELECT ... FOR UPDATE` on the affected `stock_levels` rows (in sorted order of `sku_id` to prevent deadlocks)
- The transaction verifies `available >= quantity` for every item before committing

## Expiry Sweeper

Background goroutine:
- Runs every 30 seconds
- Finds reservations where `status = 'active' AND expires_at < NOW()`
- For each: in a transaction, mark `expired`, decrement `reserved` on each item's stock level, record movements, publish `inventory.stock.reservation_expired.v1`

## Published Events

| Event | When |
|---|---|
| `inventory.stock.received.v1` | Stock received by admin |
| `inventory.stock.adjusted.v1` | Admin adjusted stock (shrinkage, correction) |
| `inventory.stock.reserved.v1` | Reservation created |
| `inventory.stock.reservation_failed.v1` | Reservation declined (shortage) |
| `inventory.stock.reservation_released.v1` | Reservation released (compensation or admin) |
| `inventory.stock.reservation_expired.v1` | Reservation timed out |
| `inventory.stock.committed.v1` | Reservation committed to order |
| `inventory.stock.depleted.v1` | Available dropped to 0 |
| `inventory.stock.restored.v1` | Available rose above 0 |
| `inventory.stock.low_threshold_reached.v1` | Available crossed below threshold |
| `inventory.back_in_stock.subscribed.v1` | Customer subscribed to a SKU |
| `inventory.back_in_stock.notified.v1` | Notification issued (consumed by yc-message) |

## Subscribed Events

| Event | Action |
|---|---|
| `product.sku.created.v1` | If `inventory_tracked = true` AND tenant has inventory enabled: create `stock_levels` row with 0 |
| `product.sku.updated.v1` | If `inventory_tracked` flipped: create or delete the stock row |
| `product.sku.deleted.v1` | Delete stock row, release any active reservations |
| `platform.tenant.provisioned.v1` | Create `tenant_inventory_config` with defaults |
| `platform.tenant.configuration_changed.v1` | Update `inventory_enabled` if present |
| `platform.tenant.deleted.v1` | Delete all tenant inventory |
| `identity.user.deleted.v1` | Delete `back_in_stock_subscriptions` for that user; publish `platform.gdpr.cascade_completed.v1` |

## Configuration

```yaml
yc-inventory:
  listen: :8080
  database:
    url: postgres://yc_inventory:...@...
  reservation:
    default_ttl_minutes: 15
    min_ttl_minutes: 5
    max_ttl_minutes: 30
    sweeper_interval_seconds: 30
  nats:
    url: nats://...
  log_level: info
```

## Dependencies on Other Services

- **None at startup**

## Called By

- `yc-checkout` (reserve, commit, release — synchronous, ADR-019)
- `yc-product` (projection sync via events)
- Shop admins (via gateway)

## GDPR Behavior

- Stock levels contain no PII
- `back_in_stock_subscriptions` is PII (contains `user_id`)
- On `identity.user.deleted.v1`: delete rows for that user; publish `platform.gdpr.cascade_completed.v1`
- Stock movements may reference `actor_id` for admin actions — admin IDs are retained per audit requirements, pseudonymized on deletion via yc-identity's pseudonym mapping

## Open Questions / Future Scope

- Multi-warehouse / multi-location stock
- Safety stock / buffer per SKU
- Backorder support (ADR-012 explicitly deferred)
- Serialized inventory (per-unit tracking for high-value items)
- Lot tracking (batch numbers, expiry dates)
- Transfer orders between locations
- Automated reorder points
- Stock take / cycle count workflows
