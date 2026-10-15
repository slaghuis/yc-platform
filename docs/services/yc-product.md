# yc-product — Service Requirement

## Overview

`yc-product` is the catalog service. It owns suppliers, products, variants, SKUs, categories, and the faceted search that drives discovery. It is deliberately **PII-free**: popularity and personalization inputs come via events from yc-orders, yc-review, yc-cart, and yc-preference — assembled into search projections.

## Location in Monorepo

```
/services/yc-product/
```

## Responsibilities

1. Maintain a list of suppliers per tenant
2. Maintain a catalog of products per supplier
3. Maintain variants per product (color, size, etc.)
4. Maintain SKUs (product + variant combination)
5. Maintain a category hierarchy per tenant
6. Attach images (by `image_id` from yc-image) to products and SKUs
7. Expose CRUD APIs for suppliers (system_admin), products/SKUs (shop_admin, system_admin)
8. Provide a faceted search API, contextualized by actor role
9. Maintain a search projection that incorporates popularity (from orders), favorites (from orders per customer), reviews, and price range
10. Emit lifecycle events for every mutation

## Multi-Tenancy

- All catalog data is tenant-scoped
- Suppliers are tenant-scoped (one supplier may be independently defined per tenant)
- Product data (name, description, imagery) is **not shared across tenants** in v1

## Domain Model

```
Supplier
  ├── Product
  │     ├── Variant (axis: color, size, ...)
  │     ├── Variant
  │     ├── SKU (product × combination of variant values)
  │     ├── SKU
  │     └── ...
  └── Product
        └── ...

Category (hierarchical, DAG allowed)
  └── Product (many-to-many)
```

- A **Product** is a conceptual item ("Acme T-Shirt")
- A **Variant** is an axis of variation ("Color: Red/Blue/Green", "Size: S/M/L")
- A **SKU** is a specific sellable combination ("Acme T-Shirt, Red, M")
- Pricing lives in **yc-pricing**, keyed by `sku_id`
- Inventory lives in **yc-inventory**, keyed by `sku_id`
- An image is referenced by `image_id` — the authoritative image data lives in yc-image

## Faceted Search

### Default Facets (v1)

- **Category** (hierarchical)
- **Price range** (from yc-pricing projection; store currency)
- **Supplier**
- **Variant axes** present on filtered products (dynamic — if filtering shows products that have "Color", the Color facet appears)
- **Average rating** (from yc-review projection)
- **Popularity** (not a filter, a sort option: popular / newest / price asc / price desc / rating)
- **In stock** (true/false — from yc-inventory projection; absent if tenant inventory disabled)

### Role-Contextualized Views

Search behavior varies by actor:

- **Visitor**: default sort = popularity (shop-wide); facets as above
- **Customer**: default sort = popularity; additional facet: "My favourites" (from `favourites` projection — products the customer has purchased most)
- **Shop admin**: unfiltered access including out-of-stock and inactive products; additional facet: "Inactive"
- **System admin**: cross-tenant search (platform-wide)

### Search Projection

A **denormalized search index** is maintained inside yc-product's database (not an external search engine in v1 — Postgres full-text + GIN indexes are sufficient). The projection is kept in sync via event subscriptions:

- `order.placed.v1` → increments popularity counters per SKU, per (user, SKU) for favourites
- `review.approved.v1` → updates average rating
- `review.like.added.v1` → increments like count
- `pricing.base_price.set.v1` → updates price range
- `inventory.stock.received.v1` / `inventory.stock.depleted.v1` → updates in-stock flag
- `product.sku.*.v1` → updates SKU-level entries

The projection is **asynchronous and eventually consistent**. Acceptable for discovery; checkout always re-fetches authoritative data.

### Future: External Search Engine

When Postgres FTS becomes insufficient (likely around 100k products or complex multi-language search), introduce OpenSearch or Meilisearch as a projection consumer. Design the projection layer so this switch is transparent to the search API.

## API

### Public Endpoints (Catalog Browsing)

| Method | Path | Description | Auth |
|---|---|---|---|
| GET | `/products` | Faceted search; returns product list | any |
| GET | `/products/{id}` | Get product with variants and SKUs | any |
| GET | `/skus/{id}` | Get SKU details | any |
| GET | `/categories` | Get category tree | any |
| GET | `/suppliers` | List suppliers (minimal fields for public) | any |

### Shop Admin Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| POST | `/admin/products` | Create product | shop_admin |
| PUT | `/admin/products/{id}` | Update product | shop_admin |
| DELETE | `/admin/products/{id}` | Soft-delete product | shop_admin |
| POST | `/admin/products/{id}/variants` | Add variant axis | shop_admin |
| POST | `/admin/products/{id}/skus` | Create SKU | shop_admin |
| PUT | `/admin/skus/{id}` | Update SKU (incl. `inventory.tracked`, `tax_class`) | shop_admin |
| DELETE | `/admin/skus/{id}` | Soft-delete SKU | shop_admin |
| POST | `/admin/products/{id}/images` | Attach image | shop_admin |
| DELETE | `/admin/products/{id}/images/{image_id}` | Detach image | shop_admin |
| POST | `/admin/categories` | Create category | shop_admin |
| PUT | `/admin/categories/{id}` | Update category | shop_admin |
| DELETE | `/admin/categories/{id}` | Delete category | shop_admin |

### System Admin Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| POST | `/sysadmin/suppliers` | Create supplier | system_admin |
| PUT | `/sysadmin/suppliers/{id}` | Update supplier | system_admin |
| DELETE | `/sysadmin/suppliers/{id}` | Delete supplier | system_admin |

### Internal Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| GET | `/internal/skus/{id}` | Full SKU record (used by cart, checkout, inventory) | service |
| GET | `/internal/skus/batch` | Bulk fetch SKUs by ID | service |

### Phase 2 Implementation Notes

The contract is `services/yc-product/api/openapi.yaml`. Differences from the tables above:

- **Suppliers** are managed at `/sysadmin/suppliers` (create, list, get, update, delete). A system admin logs in on the platform host and names the shop in the body (`{"tenant_id": "shop-a", ...}`); the write runs in that shop's tenant and the event carries its `tenant_id`. A supplier referenced by any product (deleted products included) cannot be deleted (409).
- **/admin/** endpoints act on the caller's own shop and require `shop_admin`; a system admin's `_platform` token gets 403 there.
- **Variant axes** can be added only before the product has SKUs (409 afterwards): an existing SKU would have no value on the new axis. A SKU names exactly one allowed value on every axis, and no two live SKUs of a product share a combination. `product.variant.removed.v1` is not implemented.
- **Soft deletes** set `status = 'deleted'`; a deleted SKU releases its `sku_code`. Deleting a product deletes its SKUs and lists them in `product.deleted.v1`.
- **Browse** (`GET /products`) returns active products with at least one active SKU, newest first, with cursor pagination. `q` is a web-style query over the projection's `tsv` (English stemming on name and description, plain tokens for SKU code and variant values); `category` includes subcategories. Products carry SKU IDs but no prices: clients call yc-pricing's `POST /prices/evaluate`.
- **`sku_search_projection`** is rewritten in the same transaction as every product, SKU and category change. Its event-fed columns (price, stock, popularity, rating) stayed empty in Phase 2; see Phase 3 for price and popularity.
- **Images** hold yc-image IDs only; clients resolve URLs with yc-image `GET /images/{id}`. SKU images and `image.deleted.v1` handling are deferred.
- **Internal SKU endpoints** are deferred; yc-pricing learns each SKU's tax class from `product.sku.*` events (ADR-016).
- The service registers its routes with yc-gateway at startup (`gateway.internal_url`).

### Phase 3 Implementation Notes

- **Event-fed projection.** `cart.item.added.v1` adds 1 to the SKU's `popularity_score` (consumer dedup makes redelivery safe); `order.placed.v1` took over as the popularity source in Phase 7. `pricing.base_price.set.v1` sets `base_price` (minor units, store currency) and `pricing.base_price.removed.v1` clears it; `base_price_at` records the event's time so an older event never overwrites a newer one. Stock and rating columns stay empty until yc-inventory and yc-review.
- **Sort.** `GET /products` takes `sort`: `popular` (sum of the product's active SKUs' popularity), `newest`, `price_asc` or `price_desc` (lowest active SKU base price; unpriced products sort last). Ties fall back to newest. Visitors and customers default to `popular`; admins default to `newest`. The cursor carries its sort, so reusing a cursor with another sort is a 400. The response states the `sort` used.
- **Facets.** Customers get a `facets.favourites` entry with `product_count` 0: the favourites projection is fed by `order.placed.v1` in Phase 4. Other roles get no favourites facet.
- **Internal SKU endpoints** are still deferred: yc-cart validates SKUs through yc-pricing's internal evaluate (an unknown SKU is reported as `unknown_sku`) and removes lines on `product.sku.deleted.v1` and `product.deleted.v1`.

### Phase 4 Implementation Notes

- **`GET /internal/skus/batch?ids=…`** (service callers, ADR-021) returns, for each SKU, its code, product name, variant values and the digital and inventory-tracked flags; yc-checkout reads it when a session starts. Deleted SKUs come back with their status; unknown IDs are listed in `missing`.
- **Stock.** `in_stock` follows `inventory.stock.depleted.v1` and `inventory.stock.restored.v1` only, not received or adjusted: yc-inventory publishes those two whenever availability crosses zero, including when a shop turns inventory on or off. `in_stock_at` keeps an older event from overwriting a newer one. Untracked SKUs, and shops without inventory, have no stock signal and count as in stock. Products show `in_stock` (an active SKU is in stock), and `GET /products?in_stock=true` filters on it.
- **Popularity and favourites** still come from `cart.item.added.v1` and stay empty respectively; moving them to `order.placed.v1` is not done yet.

### Phase 6 Implementation Notes

- **Shipping attributes.** A SKU has `weight_grams` (per unit) and `dimensions_mm` (`{length, width, height}`, each 1 to 10 000 mm, all three or none; columns `length_mm`, `width_mm`, `height_mm`). Both are returned by the admin SKU endpoints and `GET /internal/skus/batch`. yc-checkout passes the weight to yc-shipping's quote: weight-tiered rates are offered only when every physical SKU has one (ADR-022 rule 3). Dimensions are stored for future carrier drivers and not used yet.

### Phase 7 Implementation Notes

- **Popularity and favourites** come from `order.placed.v1`, which replaces `cart.item.added.v1` (no longer subscribed): each SKU of an order gains 1 popularity point, and a customer's order (not a guest's) records its products in `customer_favourites` (per customer and product: order count and last order time; resolved from the SKUs). `GET /products?favourites=true` (customers only; 400 otherwise) keeps the customer's ordered products, and `facets.favourites.product_count` counts those still active, regardless of the other filters. Likes are not favourites (ADR-024 rule 5).
- **Ratings and likes** are per product in `product_review_stats` (`rating_count`, `rating_sum`, `like_count`, `version`), from the `product_stats` carried by `review.submitted/edited/approved/rejected/deleted.v1` and `review.like.added/removed.v1`; a lower or equal version is ignored (ADR-024 rule 4). The unused per-SKU `avg_rating`, `rating_count` and `like_count` columns were dropped. Every product summary has `reviews` (`rating_count`, `rating_average` when rated, `like_count`), and `sort=rating` orders by the average (to three decimals), unrated products last, ties newest first.
- **`identity.user.deleted.v1`** deletes the customer's favourites (GDPR cascade, ADR-025).

## Database Schema

Database: **`yc_product`**

```sql
CREATE TABLE suppliers (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       TEXT NOT NULL,
    name            TEXT NOT NULL,
    contact         JSONB NOT NULL DEFAULT '{}',
    status          TEXT NOT NULL DEFAULT 'active',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE products (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       TEXT NOT NULL,
    supplier_id     UUID REFERENCES suppliers(id),
    name            TEXT NOT NULL,
    description     TEXT,
    is_digital      BOOLEAN NOT NULL DEFAULT FALSE,
    status          TEXT NOT NULL DEFAULT 'active',    -- 'active','inactive','deleted'
    attributes      JSONB NOT NULL DEFAULT '{}',       -- freeform tenant attributes
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_products_tenant ON products (tenant_id);
CREATE INDEX idx_products_supplier ON products (supplier_id);

CREATE TABLE product_variants (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    product_id      UUID NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    axis_name       TEXT NOT NULL,          -- 'color','size'
    values          JSONB NOT NULL,         -- ["Red","Blue"]
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE skus (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    product_id      UUID NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    tenant_id       TEXT NOT NULL,
    sku_code        TEXT NOT NULL,
    variant_values  JSONB NOT NULL DEFAULT '{}',   -- {"color":"Red","size":"M"}
    inventory_tracked BOOLEAN NOT NULL DEFAULT TRUE,
    tax_class       TEXT NOT NULL,          -- references tenant tax config in yc-pricing
    weight_grams    INT,
    length_mm       INT,                     -- dimensions for shipping, all or none
    width_mm        INT,
    height_mm       INT,
    status          TEXT NOT NULL DEFAULT 'active',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, sku_code)
);
CREATE INDEX idx_skus_tenant ON skus (tenant_id);
CREATE INDEX idx_skus_product ON skus (product_id);

CREATE TABLE categories (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       TEXT NOT NULL,
    parent_id       UUID REFERENCES categories(id),
    name            TEXT NOT NULL,
    slug            TEXT NOT NULL,
    sort_order      INT NOT NULL DEFAULT 0,
    UNIQUE (tenant_id, slug)
);

CREATE TABLE product_categories (
    product_id      UUID NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    category_id     UUID NOT NULL REFERENCES categories(id) ON DELETE CASCADE,
    PRIMARY KEY (product_id, category_id)
);

CREATE TABLE product_images (
    product_id      UUID NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    image_id        UUID NOT NULL,          -- reference to yc-image
    sort_order      INT NOT NULL DEFAULT 0,
    is_primary      BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (product_id, image_id)
);

CREATE TABLE sku_images (
    sku_id          UUID NOT NULL REFERENCES skus(id) ON DELETE CASCADE,
    image_id        UUID NOT NULL,
    sort_order      INT NOT NULL DEFAULT 0,
    PRIMARY KEY (sku_id, image_id)
);

-- Search projection
CREATE TABLE sku_search_projection (
    sku_id              UUID PRIMARY KEY,
    tenant_id           TEXT NOT NULL,
    product_id          UUID NOT NULL,
    supplier_id         UUID,
    category_ids        UUID[],
    name                TEXT NOT NULL,
    description         TEXT,
    tsv                 TSVECTOR,            -- full-text search vector
    variant_values      JSONB,
    base_price          BIGINT,              -- minor units in store currency
    in_stock            BOOLEAN,
    popularity_score    BIGINT NOT NULL DEFAULT 0,
    avg_rating          NUMERIC(3,2),
    rating_count        INT NOT NULL DEFAULT 0,
    like_count          INT NOT NULL DEFAULT 0,
    status              TEXT NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_sku_search_tsv ON sku_search_projection USING GIN (tsv);
CREATE INDEX idx_sku_search_tenant ON sku_search_projection (tenant_id);

CREATE TABLE customer_favourites_projection (
    tenant_id       TEXT NOT NULL,
    user_id         TEXT NOT NULL,
    sku_id          UUID NOT NULL,
    purchase_count  INT NOT NULL DEFAULT 0,
    last_purchased  TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, user_id, sku_id)
);
CREATE INDEX idx_favourites_user ON customer_favourites_projection (tenant_id, user_id);

CREATE TABLE outbox ( ... );
CREATE TABLE processed_events ( ... );

ALTER TABLE products ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON products
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform');
-- similar on all tenant-scoped tables
```

## Published Events

| Event | When |
|---|---|
| `product.supplier.created.v1` | Supplier created |
| `product.supplier.updated.v1` | Supplier updated |
| `product.supplier.deleted.v1` | Supplier deleted |
| `product.created.v1` | Product created |
| `product.updated.v1` | Product updated |
| `product.deleted.v1` | Product soft-deleted |
| `product.sku.created.v1` | SKU created (carries `inventory.tracked`, `tax_class`) |
| `product.sku.updated.v1` | SKU updated |
| `product.sku.deleted.v1` | SKU soft-deleted |
| `product.variant.added.v1` | Variant axis added |
| `product.variant.removed.v1` | Variant axis removed |
| `product.image_attached.v1` | Image linked to product/SKU |
| `product.image_detached.v1` | Image unlinked |
| `product.category.created.v1` | Category created |
| `product.category.updated.v1` | Category updated |
| `product.category.deleted.v1` | Category deleted |

## Subscribed Events

| Event | Action |
|---|---|
| `pricing.base_price.set.v1` | Update `sku_search_projection.base_price` |
| `pricing.base_price.removed.v1` | Clear `sku_search_projection.base_price` |
| `cart.item.added.v1` | Phase 3 to 6 only: incremented `popularity_score`; replaced by `order.placed.v1` in Phase 7 |
| `pricing.promotion.started.v1` | Flag affected SKUs in projection (on sale) |
| `pricing.promotion.ended.v1` | Unflag |
| `inventory.stock.depleted.v1` | `in_stock = false` for affected SKU |
| `inventory.stock.restored.v1` | `in_stock = true` for affected SKU |
| `review.submitted/edited/approved/rejected/deleted.v1` | Store the product's `product_stats` if newer (`product_review_stats`) |
| `review.like.added.v1`, `review.like.removed.v1` | Same |
| `order.placed.v1` | Increment `popularity_score` per SKU; record the customer's products in `customer_favourites` |
| `platform.tenant.deleted.v1` | Delete all tenant catalog |
| `identity.user.deleted.v1` | Delete rows in `customer_favourites_projection` for that user; publish `platform.gdpr.cascade_completed.v1` |
| `image.deleted.v1` | Remove references from `product_images`, `sku_images` |

## Configuration

```yaml
yc-product:
  listen: :8080
  database:
    url: postgres://yc_product:...@...
  search:
    max_results_per_page: 50
    default_page_size: 20
    facet_cache_ttl_seconds: 60
  nats:
    url: nats://...
  log_level: info
```

## Dependencies on Other Services

- **yc-image** (soft; images are referenced, not inlined)
- **yc-pricing** (via events for price in search)
- **yc-inventory** (via events for stock in search)
- **yc-review** (via events for ratings in search)
- **yc-orders** (via events for popularity in search)

None are startup-blocking — the projection is eventually consistent.

## Called By

- Clients (browse, search)
- `yc-cart`, `yc-checkout` (SKU validation)
- `yc-inventory`, `yc-pricing` (SKU existence checks)

## GDPR Behavior

- The main catalog contains **no PII**
- The `customer_favourites_projection` is PII (contains `user_id` → purchase history)
- On `identity.user.deleted.v1`: delete all rows for that `user_id` in `customer_favourites_projection`; publish `platform.gdpr.cascade_completed.v1`

*Phase 8:* Implemented as described; popularity counts no one and stays. The action is reported as `deleted`.

## Open Questions / Future Scope

- Product bundles (one SKU composed of other SKUs)
- Subscription products (recurring)
- Shared/global catalog across tenants (franchise model)
- Multi-language product content
- External search engine integration (OpenSearch/Meilisearch)
- SEO metadata per product/category
- Related products / "frequently bought together" (needs explicit modeling)
- Product import/export CSV
- Supplier portal (suppliers self-manage their products)
