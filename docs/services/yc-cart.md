# yc-cart — Service Requirement

## Overview

`yc-cart` maintains per-customer carts. A tenant may enable multiple cart types (shopping cart, wishlist, saved-for-later), and items can be moved between carts. Carts are the handoff point to yc-checkout: a checkout session is initiated from a cart snapshot.

## Location in Monorepo

```
/services/yc-cart/
```

## Responsibilities

1. Maintain carts per customer, per tenant, per cart type
2. Support multiple cart types per tenant (configurable: shopping, wishlist, etc.)
3. Add, remove, update quantity for items
4. Move items between carts (e.g., wishlist → shopping cart)
5. Merge visitor cart into customer cart on registration/login
6. Snapshot prices at the time of add (price-at-add) for display and change-detection
7. Detect price changes and signal them to the frontend
8. Emit cart activity events for analytics and abandoned cart recovery
9. Clear carts on successful checkout
10. Enforce cart limits (max items, max quantity per SKU — tenant-configurable)

## Multi-Tenancy

- Carts are scoped to `(tenant_id, user_id, cart_type)`
- A user has at most one cart per `cart_type` per tenant
- Tenants configure available cart types; v1 defaults: `shopping`, `wishlist`

## Cart Types

Tenant-configurable. Each type has properties:

```yaml
cart_types:
  shopping:
    convertible: true          # eligible for checkout
    merge_on_login: true       # visitor cart merges on login
    max_items: 100
    expires_after_days: 30     # 0 = never
  wishlist:
    convertible: false
    merge_on_login: true
    max_items: 500
    expires_after_days: 0
```

Only `convertible` carts can start a checkout.

## Visitor Carts

- Visitors have carts keyed by their visitor token's `sub` claim
- On registration/login: all visitor carts for the tenant are merged into the authenticated user's carts of the same type
- Merge rule: for duplicate SKUs, **sum quantities** (capped at per-SKU max)
- Merged visitor carts are deleted after merge

## Price Snapshotting

- When an item is added, yc-cart calls yc-pricing for the current display price and stores it on the cart line as `price_at_add`
- On every cart read, yc-cart calls yc-pricing for the current price and compares
- If different, the response includes a `price_changed: true` flag on the line (and the current price), but the stored `price_at_add` is not overwritten
- The frontend decides UX: show "price changed" badge, update the stored snapshot, etc.
- Note: `price_at_add` is **not authoritative** at checkout — yc-checkout re-prices authoritatively

## Cart Expiry

- Shopping cart items older than `expires_after_days` are removed by a daily sweep
- Expiry emits `cart.item.expired.v1` for analytics
- Wishlist items by default never expire

## Abandoned Cart Detection

- A `shopping` cart with items that has been idle for N hours (tenant-configurable, default 24h) emits `cart.abandoned.v1` once
- Re-emits on further activity + re-abandonment (debounce 24h)
- yc-message subscribes for recovery emails (if customer has consented)

## API

### Public / Authenticated Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| GET | `/carts` | List my carts | any |
| GET | `/carts/{type}` | Get cart of type | any |
| POST | `/carts/{type}/items` | Add item | any |
| PUT | `/carts/{type}/items/{sku_id}` | Set quantity | any |
| DELETE | `/carts/{type}/items/{sku_id}` | Remove item | any |
| POST | `/carts/{type}/items/{sku_id}/move` | Move to another cart | any |
| DELETE | `/carts/{type}` | Clear cart | any |

### Internal Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| GET | `/internal/carts/{user_id}/{type}` | Fetch a cart (used by yc-checkout) | service |
| POST | `/internal/carts/{user_id}/{type}/clear` | Clear cart after successful checkout | service |
| POST | `/internal/carts/merge` | Merge visitor cart into user cart (called post-login) | service |

### Admin Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| GET | `/admin/cart-types` | List configured cart types | shop_admin |
| PUT | `/admin/cart-types` | Configure cart types | shop_admin |
| GET | `/admin/abandoned-carts` | List abandoned carts (for recovery campaigns) | shop_admin |

### Phase 3 Implementation Notes

The contract is `services/yc-cart/api/openapi.yaml`. Implemented: the public endpoints above for visitors and customers (`/carts` route; other actors get 403). Internal endpoints (Phase 4, yc-checkout), admin endpoints, expiry, abandoned-cart detection and the GDPR cascade come later.

- **Cart types** come from configuration (`cart_types`): `shopping` (convertible, merge on login, 100 lines, 99 per SKU) and `wishlist` (merge on login, 500 lines, 99 per SKU). Per-tenant configuration (`tenant_cart_config`) arrives with the admin API. An unknown type is 404. `GET /carts` lists every configured type, created or not; a cart is created (`cart.created.v1`) by its first add.
- **Price snapshot.** Adding a SKU prices quantity 1 through `POST /internal/prices/evaluate` on yc-pricing, called through yc-gateway's internal listener with a service token (ADR-021, client `yc-cart`, `YC_CART_SERVICE_SECRET`). A new line stores the **gross unit price in the store currency**; adding to an existing line sums the quantity and keeps the snapshot. An unknown SKU is 404; a SKU without a price is 409; going over `max_items` or `max_quantity` is 409.
- **Change detection on read.** Every read re-prices all lines (batches of 100) and returns, per line, the snapshot (`unit_price`), the `current_unit_price`, `line_total` and `price_changed`, plus the cart's `subtotal` at current prices and `price_changed` if any line changed. Snapshots are never overwritten. Lines that can no longer be priced are `available: false` with the reason and left out of the subtotal.
- **Display currency** is converted on read and never stored: the `display_currency` query parameter, then `X-YC-Display-Currency`, then the customer's preference, which yc-cart keeps in `display_currencies` from `preference.currency.changed.v1` (an older `changed_at` is ignored). The response's `display` block carries the rate and converted amounts; it is absent for the store currency or when yc-pricing has no rate (yc-cart retries without the display currency rather than failing).
- **Visitor merge** runs on `identity.user.logged_in.v1` for customers with a `previous_visitor_id` (password or OAuth login). For each type with `merge_on_login`, the visitor's lines merge into the customer's cart: duplicate SKUs sum (capped at `max_quantity`) and keep the customer's snapshot; new lines are taken oldest first until `max_items`. The visitor cart is then deleted and `cart.merged.v1` published, so a redelivered login finds nothing to merge. Registration keeps the visitor's user ID, so the cart simply stays with the new customer and no merge is needed.
- **Moves** keep the snapshot on a new destination line and publish `cart.item.moved.v1`; `quantity` defaults to the whole line.
- **Deleted SKUs** (`product.sku.deleted.v1`, and the `sku_ids` of `product.deleted.v1`) are removed from every cart in the shop with `cart.item.removed.v1` reason `sku_deleted`.
- **Not yet subscribed:** `identity.user.registered.v1` (not needed, see above), `identity.user.deleted.v1`, inventory, promotions, tenant lifecycle and checkout events.

### Phase 4 Implementation Notes

- **`GET /internal/carts/{user_id}/{type}`** (service callers, ADR-021) returns the cart's lines unpriced, with whether the type is convertible; yc-checkout snapshots them when a session starts. A cart not created yet has no lines. The clear and merge internal endpoints are not built: merging runs on the login event (Phase 3) and clearing on the checkout event below.
- **Checkout completion.** On `checkout.completed.v1` yc-cart takes the checked-out quantities off the cart's lines, rather than emptying it, so items added during checkout stay. It records each session in `cart_conversions`, so a redelivered event changes nothing, and publishes `cart.converted.v1`.

## Database Schema

Database: **`yc_cart`**

```sql
CREATE TABLE tenant_cart_config (
    tenant_id       TEXT PRIMARY KEY,
    cart_types      JSONB NOT NULL,         -- type definitions
    abandoned_hours INT NOT NULL DEFAULT 24,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE carts (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       TEXT NOT NULL,
    user_id         TEXT NOT NULL,          -- visitor sub or user_id
    user_type       TEXT NOT NULL,          -- 'visitor','customer'
    cart_type       TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_activity   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    abandoned_at    TIMESTAMPTZ,            -- set when abandonment event fired
    UNIQUE (tenant_id, user_id, cart_type)
);
CREATE INDEX idx_carts_abandoned ON carts (tenant_id, last_activity)
    WHERE abandoned_at IS NULL;

CREATE TABLE cart_items (
    cart_id         UUID NOT NULL REFERENCES carts(id) ON DELETE CASCADE,
    sku_id          UUID NOT NULL,
    quantity        INT NOT NULL CHECK (quantity > 0),
    price_at_add    BIGINT NOT NULL,        -- minor units in display currency at time of add
    currency_at_add TEXT NOT NULL,
    added_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (cart_id, sku_id)
);

CREATE TABLE outbox ( ... );
CREATE TABLE processed_events ( ... );

ALTER TABLE carts ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON carts
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform');
-- similar on cart_items via join
```

## Published Events

| Event | When |
|---|---|
| `cart.created.v1` | New cart (first add) |
| `cart.item.added.v1` | Item added |
| `cart.item.removed.v1` | Item removed |
| `cart.item.quantity_changed.v1` | Quantity updated |
| `cart.item.moved.v1` | Item moved between carts |
| `cart.item.expired.v1` | Item removed by expiry sweep |
| `cart.cleared.v1` | Cart cleared (manually or post-checkout) |
| `cart.merged.v1` | Visitor cart merged into customer cart |
| `cart.abandoned.v1` | Idle beyond threshold |
| `cart.converted.v1` | Cart handed off to checkout successfully |

## Subscribed Events

| Event | Action |
|---|---|
| `identity.user.logged_in.v1` | Trigger merge of visitor carts (if a visitor session preceded login: `previous_visitor_id`) |
| `identity.user.deleted.v1` | Delete all user carts; publish `platform.gdpr.cascade_completed.v1` |
| `product.sku.deleted.v1` | Remove the SKU from all carts; emit `cart.item.removed.v1` |
| `product.deleted.v1` | Remove the product's SKUs (`sku_ids`) from all carts; emit `cart.item.removed.v1` |
| `product.sku.updated.v1` | No action on cart content, but freshen snapshots on next read |
| `pricing.base_price.set.v1` | No immediate action (change detection on read) |
| `pricing.promotion.started.v1` | No immediate action |
| `inventory.stock.depleted.v1` | Flag affected cart lines as "unavailable" (not removed; UX decides) |
| `inventory.stock.restored.v1` | Clear "unavailable" flag |
| `preference.currency.changed.v1` | Record the customer's display currency for reads (cart contents unchanged) |
| `platform.tenant.provisioned.v1` | Create default `tenant_cart_config` |
| `platform.tenant.deleted.v1` | Delete all tenant carts |
| `checkout.completed.v1` | Clear the source cart |

## Configuration

```yaml
yc-cart:
  listen: :8080
  database:
    url: postgres://yc_cart:...@...
  defaults:
    cart_types:
      shopping:
        convertible: true
        merge_on_login: true
        max_items: 100
        expires_after_days: 30
      wishlist:
        convertible: false
        merge_on_login: true
        max_items: 500
        expires_after_days: 0
    abandoned_hours: 24
  sweeper:
    expiry_interval_hours: 24
    abandoned_check_interval_minutes: 15
  nats:
    url: nats://...
  log_level: info
```

## Dependencies on Other Services

- **yc-pricing** (for price snapshotting and change detection)
- **yc-product** (for SKU validation on add)
- **yc-preference** (for display currency)

## Called By

- Clients (via gateway)
- `yc-checkout` (reads cart to start a session; signals clear on completion)

## GDPR Behavior

- Carts are PII (linked to user_id)
- On `identity.user.deleted.v1`: delete all carts for the user; publish `platform.gdpr.cascade_completed.v1`

## Open Questions / Future Scope

- Shared/collaborative carts (gift registries)
- Scheduled cart reminders ("you have N items")
- Cart-level notes (e.g., "gift wrap this item")
- Save cart for later as a named list
- Cart item configurability (customization text, engraving)
- Guest cart persistence by email (recovery link)
