# Related products and also-bought

## Overview
**Status:** Implemented
**GitHub Issues:** [#25](https://github.com/slaghuis/yc-platform/issues/25), [#26](https://github.com/slaghuis/yc-platform/issues/26)
**Priority:** High (Wisefood PDP merchandising)
**Repository:** yc-platform only. yc-product only (both features).
**Work package:** WP2 (PDP merchandising)

## Requirement

Storefront PDPs need two distinct merchandising rails:

1. **Editorial related products (#25)** — a shop admin curates zero or more other products that “go with” this one.
2. **Also-bought (#26)** — a derived ranking of other products that customers who bought this product also bought, from confirmed orders.

They must not share storage or APIs. Related is explicit merchandising; also-bought is collaborative filtering over `order.placed.v1`. Neither replaces categories, banners, or future bundles.

### Decisions proposed for this draft

| Topic | Proposal |
|---|---|
| Service ownership | **yc-product** for both (same PDP surface; product already consumes `order.placed.v1` for popularity/favourites) |
| Related public shape | Ordered `related_product_ids` on `GET /products/{id}` (IDs only; client batch-fetches or uses also-bought-style summaries later if needed) |
| Related admin API | `PUT /admin/products/{id}/related` replaces the full ordered list (same replace-all pattern as `category_ids`) |
| Related cardinality | 0..`MaxRelatedProducts` (**20**) |
| Related inactive/deleted targets | Public get **omits** inactive and deleted targets; admin put rejects unknown/deleted ids; soft-delete of a target **removes** inbound related links |
| Also-bought API | `GET /products/{id}/also-bought?limit=` — public; returns **ProductSummary** items ordered most → least |
| Also-bought metric | Count of **distinct placed orders** (guest and customer) whose lines include both the seed product and the other product (resolve SKU → product). Seed excluded. |
| Also-bought limit | Default **8**, max **24** |
| Also-bought cold start | Fewer than `limit` or empty is valid; no synthetic filler |
| Also-bought consistency | Eventually consistent projection updated on `order.placed.v1` (same class as popularity) |
| Self / inactive | Never return the seed; omit inactive and deleted products |

## Current behaviour (as-built)

- `GET /products/{id}` returns variants, SKUs, categories, images — no related collection.
- `order.placed.v1` already updates per-SKU popularity and customer favourites in yc-product; there is no co-purchase table.
- yc-analytics has `fact_order_item` but no public storefront recommendation API; keeping also-bought in yc-product avoids a cross-service PDP hop and matches favourites/popularity ownership.
- Related / “frequently bought together” is listed under yc-product Open Questions / Future Scope.

## Part A — Editorial related products (#25)

### Domain

```sql
CREATE TABLE product_related (
    tenant_id         TEXT NOT NULL,
    product_id        UUID NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    related_product_id UUID NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    sort_order        INT NOT NULL DEFAULT 0,
    PRIMARY KEY (product_id, related_product_id),
    CHECK (product_id <> related_product_id)
);
CREATE INDEX idx_product_related_related ON product_related (related_product_id);
```

Links are directed: A→B does not imply B→A. Ordering is `sort_order` ascending, then `related_product_id` for ties.

### API

| Method | Path | Who | Behaviour |
|---|---|---|---|
| PUT | `/admin/products/{id}/related` | shop_admin | Body: `{ "product_ids": [uuid, …] }` — full replace, order preserved. Empty list clears. Repeated ids are collapsed (first occurrence keeps its place); rejects self, non-UUID, unknown, deleted, or other-tenant ids (`400`). Inactive targets are allowed (admin may pre-stage). Cap 20 distinct ids. Responds `200` with `{ "product_ids": [...] }` (the stored list). Publishes `product.related.updated.v1`. |
| GET | `/products/{id}` | any (existing) | Response adds `related_product_ids` (ordered). Public and customer views list only targets that are **active** with at least one active SKU. Shop admins see the stored list including inactive targets (same visibility rule as inactive products elsewhere). Field is always present (possibly `[]`). |

No separate detach route: omit an id from the PUT list. Create/update product bodies do **not** take related ids (keeps `ProductInput` stable).

### Events

| Event | When |
|---|---|
| `product.related.updated.v1` | Admin replaced the related list (`product_id`, `related_product_ids`, `updated_at`) |

### Soft-delete rules

- Soft-delete of the **source** product cascades related rows (FK / explicit delete with the product).
- Soft-delete of a **target** deletes rows where `related_product_id` is that product (no dangling public ids).
- Reactivating a target does not restore removed links; the admin must PUT again.

## Part B — Also-bought (#26)

### Domain

Projection (tenant-scoped), maintained from `order.placed.v1`:

```sql
CREATE TABLE product_also_bought (
    tenant_id          TEXT NOT NULL,
    product_id         UUID NOT NULL,
    other_product_id   UUID NOT NULL,
    order_count        BIGINT NOT NULL DEFAULT 0,
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, product_id, other_product_id),
    CHECK (product_id <> other_product_id)
);
CREATE INDEX idx_also_bought_rank ON product_also_bought (tenant_id, product_id, order_count DESC);
```

On each `order.placed.v1`, resolve line SKUs to distinct product ids in the order. For every unordered pair `{A,B}` among those products, increment both `(A,B)` and `(B,A)` by 1 (so each direction ranks for either seed). Consumer dedup (existing processed-events pattern) makes redelivery safe.

Cancellations/refunds do **not** decrement in this package (document as known bias; can revisit with order lifecycle events later).

### API

| Method | Path | Who |
|---|---|---|
| GET | `/products/{id}/also-bought` | any (shop host) |

| Parameter | Rules |
|---|---|
| `limit` | Optional. Default 8, max 24. Invalid → `400`. |

**Response:** `{ "items": [ ProductSummary, … ] }` — same summary shape as browse (name, skus, primary image, category_ids, in_stock, reviews, …), ordered by `order_count` descending, ties by `other_product_id`. Only **active** products with an active SKU. Seed product never included. Unknown or inactive seed → `404` (same as `GET /products/{id}` for visitors).

No admin write API. No mixing with `related_product_ids`.

### Consistency

Also-bought lags orders by the usual event/outbox delay. Document in OpenAPI that empty results are normal for new catalogue items.

## Out of scope

- Bundles / “frequently bought together” kits as a sellable SKU.
- Cart-abandon or browse co-occurrence signals.
- yc-analytics public recommendation endpoints (may still use facts for admin reports later).
- Personalised “for you” rankings.
- Changes to banners, categories, or browse filters.
- yc-platform-shop / yc-platform-admin UI (follow-ups only).

## Follow-up (do not implement in this package)

- yc-platform-admin: related-products editor on the product form.
- yc-platform-shop: PDP rails for related vs also-bought; capability matrix PDP-002 / GAP-010 / OQ-004.
- yc-platform-mcp: OpenAPI sync after platform tag (ADR-028).
- Optional later: decrement also-bought on cancel/refund; admin rebuild job.

## Test plan

`go test -tags=integration -count=1 ./...` in `services/yc-product`, plus lint with integration tags.

**Related**

1. Admin PUT related list (order, cap, duplicates collapsed, reject self/unknown).
2. Public `GET /products/{id}` returns only active related ids in order.
3. Soft-delete of a related target removes it from subsequent public gets.
4. System admin receives 403 on the admin route.

**Also-bought**

1. Two products share N placed orders → also-bought ranks the peer first.
2. `limit` respected; seed absent; inactive peer omitted.
3. Cold product returns empty `items`.
4. Guest and customer orders both count.
5. Redelivered `order.placed.v1` does not double-count (dedup).

## Acceptance

- [x] Shop admin can replace the ordered related-product list for a product (0..20).
- [x] Public product get exposes active `related_product_ids` in that order.
- [x] Soft-deleted / inactive related targets do not appear on public get; target delete cleans links.
- [x] `GET /products/{id}/also-bought` returns up to `limit` ProductSummaries by co-purchase `order_count`.
- [x] Metric, cold start, and eventual consistency are documented in OpenAPI.
- [x] Related and also-bought are separate; OpenAPI + service doc + event schema updated; ADR-028 sync called out.

## Open for review

1. Also-bought ownership in **yc-product** (vs yc-analytics) — confirm.
2. Related public payload = **ids only** on getProduct (vs embedded summaries).
3. Also-bought metric = **distinct orders** (vs distinct customers or line-quantity weighting).
4. No decrement on cancel/refund in this package — confirm.
5. Cap **20** related / default **8** also-bought (max 24) — confirm or change.
