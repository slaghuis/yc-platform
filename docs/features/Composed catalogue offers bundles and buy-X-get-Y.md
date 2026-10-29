# Composed catalogue offers — bundles and buy-X-get-Y

## Overview
**Status:** Approved (implemented; awaiting release tag)
**GitHub Issues:** [#34](https://github.com/slaghuis/yc-platform/issues/34) (product bundles), [#31](https://github.com/slaghuis/yc-platform/issues/31) (sale pricing + buy-X-get-Y)
**Priority:** Low / deferred (Wisefood: **GAP-010** / **BNDL-001**; **GAP-002** / **PRICE-002**, **PRICE-003**)
**Repository:** yc-platform — primarily **yc-product** (composition / sellable identity) and **yc-pricing** (sale overlays + campaign rules); stock cascade touches **yc-inventory**; cart/checkout/orders must honour authoritative prices and line snapshots. Storefront/BFF consumes OpenAPI ([ADR-028](../adr/ADR-028%20Frontend%20Stack%20and%20API%20Contract%20Distribution.md)).
**Work package:** TBD — sequence relative to Wisefood release trains (see Decisions)

## Requirement

Shop admins need to **sell more than one catalogue product as a single customer-facing offer**. Issues [#34](https://github.com/slaghuis/yc-platform/issues/34) and [#31](https://github.com/slaghuis/yc-platform/issues/31) both ask for that idea; they differ in **how permanent and how priced** the composition is.

| Mode | Issue | Customer sees | Platform truth |
|---|---|---|---|
| **Fixed bundle / kit** | #34 | One product (variants/SKUs, categories, PDP) composed of ≥2 existing products | A new **virtual catalogue product** whose SKUs expand to component SKUs for stock, fulfilment, and audit |
| **Buy-X-get-Y** | #31 | Still separate PDPs / lines, but adding A unlocks a discount on admin-picked target B | A **pricing campaign rule** over existing SKUs (qualifying + target), not a new product |
| **Sale / promotional unit price** | #31 | Catalogue and PDP show a promotional price distinct from base | A **time-bounded overlay** on base price in the evaluate pipeline |

This FR treats them as **one composed-offers programme** with two delivery mechanisms (catalogue composition vs pricing rules), so we do not invent two incompatible models for “these products sell together”.

### Why it matters

- Wisefood needs curated meal-style bundles later (**GAP-010**) and promotional merchandising (**GAP-002**).
- Related / also-bought ([Related products and also-bought](./Related%20products%20and%20also-bought.md)) are merchandising rails only — they do **not** change price or stock. ([docs/services/yc-product.md](../services/yc-product.md) still lists product bundles under Future Scope.)
- Shops must not invent local “bundle SKUs” or BFF-side discounts that inventory, tax, and checkout cannot honour. Pricing remains authoritative in **yc-pricing** (evaluate pipeline + tax per [ADR-016](../adr/ADR-016%20Tax%20within%20Pricing%20—%20Multiple%20Tax%20Rates%20per%20Product.md); store currency per [ADR-014](../adr/ADR-014%20Single%20Store%20Currency,%20Display%20Currency%20Conversion.md)).

### Current behaviour (as-built)

| Surface | Today |
|---|---|
| Catalogue | Product → variants → SKUs; pricing and inventory keyed by `sku_id` |
| Bundles | Unsupported (`bundle` → 0); future scope in yc-product |
| Promotions | Pipeline designed (SKU/category/cart/code evaluators) but **not** in the live evaluate path; base + tax only |
| Related / also-bought | Editorial + co-purchase rails; **no** discount application |
| Analytics | Promotion effectiveness aggregates deferred |

---

## Shared concepts (common ground)

Both modes need a clear answer to the same questions:

1. **Composition** — which existing products/SKUs participate, and in what quantities?
2. **Customer line shape** — one cart line (bundle SKU) vs multiple lines (qualifying + discounted target)?
3. **Stock** — when is the offer unsellable because a component is unavailable?
4. **Price authority** — who computes the payable amount, and how is it snapshotted on checkout/order?
5. **Lifecycle** — what happens when a component is soft-deleted, inactivated, or removed from the catalogue?
6. **Admin surface** — shop-admin configures the composition; OpenAPI is the contract of record.

Locked principle:

> **Catalogue composition (#34) owns identity and stock semantics. Pricing rules (#31) own money. Neither service invents the other's truth.**

---

## Part A — Fixed product bundles (#34)

### Intent

Admin composes a **new sellable product** from ≥2 existing catalogue products. The bundle is first-class: variants, SKUs, categories, browse/PDP, cart and checkout — same paths as ordinary products where applicable.

### Model (locked)

| Topic | Decision |
|---|---|
| Ownership | **yc-product** owns bundle definition and the virtual product/SKU records |
| Sellable unit | Bundle has its own `product_id` / `sku_id`s (customer adds **one** SKU to the cart) |
| Cart / checkout line | **One line** for the bundle SKU, with an **internal component snapshot** (sku ids, qty, names). Do **not** explode into multiple payable lines at checkout |
| Future packing list | When shipping packing lists land, **then** break the bundle open into an item list for fulfilment — out of MVP; snapshot must already carry enough detail to support that later |
| Composition depth | **Flat only** — a bundle’s components are ordinary (non-bundle) catalogue SKUs; no nested kits |
| Components | Each bundle SKU maps to an ordered list of `{component_sku_id, quantity}`; MVP: admin picks concrete component SKUs per bundle SKU (no auto-expand of all variant combos) |
| Pricing | Bundle SKU has a **base price** in yc-pricing like any SKU (not auto-sum in MVP). Optional later: “price = sum of components − X%” as a pricing rule |
| Stock cascade | Bundle SKU is **out of stock** when any required tracked component cannot satisfy its quantity. Untracked components documented. Projection `in_stock` updates from inventory events |
| Mid-checkout stock | Component shortfall fails **identically to a normal SKU stock conflict** (same reservation / conflict path as a plain SKU) |
| Catalogue-removal cascade | Soft-delete / remove of a **component product** soft-retires dependent bundle products; no orphan sellable bundles |
| Categories | Bundle product participates in category DAG like any product |
| Digital mix | MVP: **physical-only**, or require all-digital vs all-physical (no mixed digital+physical kits until revisited) |

### Admin API (sketch)

| Method | Path | Behaviour |
|---|---|---|
| POST | `/admin/bundles` | Create bundle product + initial component set (≥2 products); returns product id |
| PUT | `/admin/bundles/{product_id}` | Replace component set / variant mapping rules |
| GET | `/admin/bundles/{product_id}` | Bundle definition + component expansion |
| GET | `/products/{id}` | Existing; when product is a bundle, expose `bundle: { components: [...] }` (or `kind: bundle`) for PDP |

Exact shapes follow OpenAPI-first (ADR-028). Prefer extending product kind over a parallel catalogue.

### Acceptance (Part A)

- [x] Shop-admin can create/update/list a bundle from ≥2 catalogue products
- [x] Bundle is a first-class product with variants/SKUs and category association
- [x] Customer pays for **one** cart/checkout line; component snapshot stored for audit / future packing list
- [x] Composition is flat (components are non-bundle SKUs only)
- [x] Adding the bundle SKU uses existing cart/checkout paths; price from yc-pricing
- [x] Bundle unsellable / `in_stock=false` when any required component is out of stock (untracked rules documented)
- [x] Component stock shortfall at checkout behaves like a normal SKU stock conflict
- [x] Removing a component product from the catalogue retires dependent bundles (soft lifecycle)
- [x] OpenAPI + MCP + service docs updated

---

## Part B — Sale prices and buy-X-get-Y (#31)

### Intent

1. **Sale / promotional unit prices** on catalogue and PDP (distinct from the single base-price path).
2. **Campaign rule:** buy product/SKU A (qualifying), get an **admin-picked** product/SKU B at a configured discount.

Coupons/codes and loyalty remain out of scope (see #36 for loyalty).

### Model (locked)

| Topic | Decision |
|---|---|
| Ownership | **yc-pricing** owns sale overlays and campaign rules; evaluate pipeline applies them before tax (as already sketched in [yc-pricing](../services/yc-pricing.md)) |
| Sale price | Time-bounded overlay per SKU (or campaign membership); display endpoints and `POST /prices/evaluate` both reflect it; audit trail lists the overlay |
| Buy-X-get-Y | Rule: `{qualifying_sku_or_product, target_sku_or_product, discount_type, discount_value, window, stacking}` — **not** a new catalogue product |
| Target selection | **Any admin-picked target** — not restricted to editorial `related_product_ids`. Admin UX may still *suggest* related products, but the rule stores explicit target ids and must not silently discount editorial related |
| Cart shape | Customer still has **separate lines** for A and B; pricing attaches a discount to B (or a cart-level adjustment with line attribution) when eligibility holds |
| Eligibility | Document: quantity thresholds, whether A must remain in cart, guest vs customer, one redemption per cart vs per A unit |
| Stacking | Sale overlay on B does **not** stack with buy-X-get on B in MVP; mutual exclusion with future coupon codes documented when codes arrive |
| Checkout/order | Snapshots must show promotional amounts from evaluate — shop/BFF must not invent discounts |
| Events | Reuse/extend planned `pricing.promotion.*.v1` for search projection “on sale” flags |

### Acceptance (Part B)

- [x] Documented sale/promotional price model through the existing pricing/tax pipeline
- [x] Buy-X-get-Y rule with admin-picked targets; eligibility + stacking documented and enforced in evaluate
- [x] Catalogue/PDP can show promotional price without BFF math
- [x] Checkout and order snapshots reflect promotional amounts authoritatively
- [x] Shop-admin (or sysadmin) configuration for campaigns / sale prices
- [x] OpenAPI + MCP updated

---

## How the two parts relate (and must not collide)

```
                    ┌─────────────────────────────┐
                    │   Shop-admin “sell together” │
                    └──────────────┬──────────────┘
               ┌───────────────────┴───────────────────┐
               ▼                                       ▼
   Fixed bundle (#34)                      Campaign / sale (#31)
   yc-product virtual product              yc-pricing rule
   1 cart line → N components (snapshot)   N cart lines → discount on target
   Stock cascade on components             Stock = ordinary SKU availability
   Own base price (usually)                Overlay / % or fixed off target
```

**Do not** implement buy-X-get-Y by auto-creating hidden bundle products. **Do not** implement fixed bundles only as a cart discount with no catalogue identity — fulfilment and stock cascade need the virtual SKU (#34).

Optional later bridge: a bundle SKU’s base price could be maintained by a pricing rule (“sum of components − 10%”). That is an enhancement on top of Part A, not a substitute for it.

Abandoned-cart recovery’s optional discount continues to **depend on Part B / #31**.

---

## Decisions (locked 2026-10-29)

| Topic | Decision |
|---|---|
| One FR vs two | **One programme doc**, two delivery parts; GitHub #31 and #34 remain trackable slices |
| Bundle identity | Product `kind=bundle` with real SKUs in yc-product |
| Bundle cart line | **One payable line** + internal component snapshot |
| Future packing list | Out of MVP; when shipping packing lists exist, expand snapshot into a pick/pack item list |
| Composition depth | **Flat only** — no nested bundles |
| Bundle price | Admin sets bundle SKU base price (not auto-sum in MVP) |
| Bundle variant UX | Admin picks concrete component SKUs per bundle SKU |
| Stock cascade | Any tracked component short ⇒ bundle out of stock |
| Mid-checkout stock | **Identical** to a normal SKU stock conflict |
| Component removed | Soft-retire bundle product |
| Buy-X-get target | **Any admin-picked** product/SKU (not limited to editorial related) |
| Sale vs buy-X-get stack | No stack on the same target line in MVP |
| Coupons / loyalty | Out of scope (#36 loyalty; codes later) |
| Build order | **Relative to Wisefood release trains.** Suggested platform slice order when scheduled: **sale overlay → buy-X-get-Y → bundles**; final train assignment follows Wisefood scheduling |

---

## Out of scope (unless expanded)

- Coupon / voucher **codes**
- Loyalty / rewards ([#36](https://github.com/slaghuis/yc-platform/issues/36))
- Customer “build your own box” from arbitrary catalogue
- Split-ship of bundle components to different destinations
- Exploding the bundle into multiple payable cart/checkout lines
- Nested kits (bundle-of-bundles)
- Shipping **packing list** expansion (future; snapshot must enable it)
- Subscription / recurring kits
- Auto-priced “dynamic bundles” from also-bought

---

## Origin

- [#34](https://github.com/slaghuis/yc-platform/issues/34) — Product bundles composed from catalogue products
- [#31](https://github.com/slaghuis/yc-platform/issues/31) — Promotional / sale pricing and buy-X-get-related discount engine
- Review answers locked 2026-10-29
- Wisefood brief §5 product bundles / §5.2–5.4 promotions; fit-gap GAP-010, GAP-002
- `docs/client-api-fit-gap-analysis.md` (Deferred)
- Service future scope: [yc-product](../services/yc-product.md) bundles; [yc-pricing](../services/yc-pricing.md) promotion evaluators
