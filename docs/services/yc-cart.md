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
| `identity.user.registered.v1` | Trigger merge of visitor carts for this user |
| `identity.user.logged_in.v1` | Trigger merge of visitor carts (if a visitor session preceded login) |
| `identity.user.deleted.v1` | Delete all user carts; publish `platform.gdpr.cascade_completed.v1` |
| `product.sku.deleted.v1` | Remove the SKU from all carts; emit `cart.item.removed.v1` |
| `product.sku.updated.v1` | No action on cart content, but freshen snapshots on next read |
| `pricing.base_price.set.v1` | No immediate action (change detection on read) |
| `pricing.promotion.started.v1` | No immediate action |
| `inventory.stock.depleted.v1` | Flag affected cart lines as "unavailable" (not removed; UX decides) |
| `inventory.stock.restored.v1` | Clear "unavailable" flag |
| `preference.currency.changed.v1` | Refresh cart display currency on next read (no stored change) |
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
