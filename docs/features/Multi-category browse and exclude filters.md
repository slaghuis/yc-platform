# Multi-category browse and exclude filters

## Overview
**Status:** Implemented
**GitHub Issues:** [#23](https://github.com/slaghuis/yc-platform/issues/23), [#24](https://github.com/slaghuis/yc-platform/issues/24)
**Priority:** High (Wisefood Must: multi-hierarchy discovery and allergen exclusion)
**Repository:** yc-platform only. yc-product only.
**Work package:** WP1 (catalogue discovery)

## Requirement

A shop models **one or more category hierarchies** as separate trees in the existing category table (multiple roots). A product already belongs to **zero or more** categories via `product_categories` / `category_ids` on create and update. That membership stays.

This work extends **public browse** so a client can require membership in several categories at once (positive, AND) and exclude products associated with one or more categories (negative). Filtering is authoritative in yc-product’s search projection; storefronts must not invent allergen or multi-facet exclusion client-side.

`GET /categories` already returns every root and its descendants. Document that multi-root shape as the supported multi-hierarchy navigation contract. Banners stay one category per banner.

### Decisions locked for this draft

| Topic | Decision |
|---|---|
| Multiple positive categories | **AND** — a product must match every positive constraint |
| Match per category id | The product is in that category **or any descendant** (same subtree rule as today’s single `category`) |
| Multiple excludes | **AND NOT** each — a product matching any exclude subtree is dropped |
| Composition | `(all positives) AND NOT (any exclude)` , then existing `q`, `supplier_id`, `in_stock`, `favourites`, `sort` as today |
| Unknown / malformed UUID | `400` problem+json (same validation style as other browse params) |
| Unknown but well-formed category id | Treated as an empty subtree for that constraint (positive → no matches for that AND term; exclude → excludes nothing for that id). Do not 404 browse for a missing category id. |

### Example (Wisefood-shaped)

Roots: Lifestyle, Allergens, Product Class.

`GET /products?category={vegan}&category={beef}&exclude_category={nuts}`

Returns products that are under Lifestyle/Vegan **and** under Product Class/Beef, and **not** under Allergens/Nuts (including Nuts’ subcategories).

## Current behaviour (as-built)

- Admin create/update product accepts `category_ids` (many-to-many). Uncategorised products are valid.
- `sku_search_projection.category_ids` is rewritten with product/SKU/category changes.
- `GET /products?category={id}` keeps products whose projection `category_ids` intersects that id’s subtree.
- `GET /categories` returns `{ items: CategoryNode[] }` — multiple roots are already possible; there is no separate “hierarchy” resource.
- Banners require exactly one `category_id` (unchanged by this feature).

## API

### `GET /products` (browseProducts)

| Parameter | Change | Semantics |
|---|---|---|
| `category` | Become **repeatable** (OpenAPI array, `style: form`, `explode: true`) | Zero or more UUIDs. Each expands to self + descendants. Product must satisfy **every** listed id (AND). Omitting the param is unchanged (no positive category filter). One value preserves today’s behaviour. |
| `exclude_category` | **New**, same array style | Zero or more UUIDs. Each expands to self + descendants. Product must **not** match any listed id’s subtree. |
| `q`, `supplier_id`, `in_stock`, `favourites`, `sort`, pagination | Unchanged | Applied after category constraints. |

Duplicate ids in `category` or in `exclude_category` are ignored (dedupe). The same id in both `category` and `exclude_category` yields an empty result set (AND with NOT of the same subtree).

Cursor compatibility: a cursor remains valid only with the sort it was issued for (existing rule). Category and exclude filters may change between pages; that is the caller’s responsibility, same as changing `q` today. No new cursor fields.

OpenAPI description must state AND / AND-NOT and subtree rules explicitly so MCP and generated clients do not assume OR.

### `GET /categories` (getCategoryTree)

No response-shape change. Document in the operation description:

- `items` is the list of **root** categories for the shop (multi-hierarchy = multiple roots).
- Each root is an independent tree; there is no hierarchy id or type field in v1.
- Shops that need named hierarchies (Lifestyle, Allergens, …) create one root per hierarchy and hang children under it.

### Admin product ↔ category

No new endpoints. Attach/detach remains replace-all `category_ids` on create and update. Confirm in the service doc that zero, one, or many categories across any roots are allowed, and that browse filters use that membership.

### Banners

Unchanged. Create/update still require a single `category_id`. Banner click still means browse with one positive category (callers may add further `category` / `exclude_category` params if the storefront wants a tighter rail).

## Implementation notes

1. Extend `domain.BrowseFilter` from a single `CategoryID` to `CategoryIDs []string` and `ExcludeCategoryIDs []string`.
2. In `Catalog.Browse`, for each positive id keep the existing recursive subtree `&&` against `sp.category_ids`; **AND** those predicates together. For each exclude id, add `NOT (sp.category_ids && ARRAY(subtree…))`.
3. Cap the number of distinct positive and exclude ids per request (suggested: **16 each**) with `400` if exceeded, so a malicious query cannot explode recursive CTEs.
4. Prefer one SQL round-trip; reuse the existing recursive CTE pattern (factor if needed). No external search engine.
5. Update `docs/services/yc-product.md` Phase notes / browse description; no new ADR (tenancy and projection model unchanged). Call out the query-param change as a **compatible extension** under ADR-028 (clients that send a single `category` keep working).

## Out of scope

- OR semantics for positive categories.
- Category “types”, tags, or a separate hierarchy resource.
- Changing banner cardinality or banner ↔ category rules.
- Related products (#25) or also-bought (#26).
- Changes to yc-image, yc-platform-shop, or yc-platform-admin (follow-ups only).
- Facet payload that lists allergen counts for the current filter set (may come later; not required for Must browse).
- Soft-deleted / inactive products: browse still returns active products with an active SKU only.

## Follow-up (do not implement in this package)

- yc-platform-shop: wire multi-select and allergen exclude to the new params; refresh capability matrix (GAP-001, CAT-001/002, SRCH-002, OQ-002, OQ-014).
- yc-platform-mcp: one OpenAPI sync after the platform tag (same pattern as #17/#20).
- yc-platform-admin: multi-category product editor UX if not already using `category_ids[]`.

## Test plan

`go test -tags=integration -count=1 ./...` in `services/yc-product`, plus `golangci-lint run --build-tags=integration ./...`.

Add or extend an integration case (e.g. under the existing product browse tests) that:

1. Creates two roots (e.g. Lifestyle, Allergens) with children Vegan and Nuts.
2. Creates products: vegan-only, nuts-only, vegan+nuts, uncategorised.
3. Asserts `category={vegan}` returns vegan-only and vegan+nuts (subtree behaviour).
4. Asserts `category={vegan}&category={nuts}` returns only vegan+nuts (AND).
5. Asserts `category={vegan}&exclude_category={nuts}` returns only vegan-only.
6. Asserts `exclude_category={nuts}` alone drops nuts-only and vegan+nuts.
7. Asserts a child id matches when filtering by the parent root (subtree).
8. Asserts too many `category` / `exclude_category` values → 400.
9. Asserts a single `category` still matches today’s behaviour (compat).
10. Asserts banners still require one category and are unchanged.

Unit-cover BrowseFilter parsing / dedupe if that logic is non-trivial.

## Acceptance

- [x] A shop admin can attach a product to multiple categories across multiple roots (already true; covered by regression).
- [x] `GET /categories` is documented as a multi-root forest for multi-hierarchy navigation.
- [x] `GET /products` accepts multiple `category` values with **AND** + subtree semantics.
- [x] `GET /products` accepts one or more `exclude_category` values with subtree exclusion, composed with positives as AND NOT.
- [x] Filters run in the search projection (no full-catalogue client filter).
- [x] Banner create/update still requires a single `category_id`.
- [x] OpenAPI + service doc updated; ADR-028 sync called out for MCP/shop after tag.
- [x] Integration tests above pass.

## Review resolutions

1. Cap of **16** ids per param — kept.
2. Missing category UUID → empty contribution — kept.
3. Parameter names `category` + `exclude_category` — kept for compat.
