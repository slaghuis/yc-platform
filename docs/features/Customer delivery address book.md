# Customer delivery address book

## Overview
**Status:** Approved (implemented; awaiting release tag)
**GitHub Issues:** [#32](https://github.com/slaghuis/yc-platform/issues/32)
**Priority:** High (Wisefood §8 Address book / Customer profile; fit-gap GAP-007 / CUST-005, GAP-008 / CUST-006)
**Repository:** yc-platform — primarily **yc-identity**; **yc-orders** (lock + snapshot id); **yc-checkout** (select / auto-save). Storefront/BFF consumes OpenAPI (ADR-028).
**Work package:** WP4d (after WP4c / #30)

## Requirement

Enable authenticated **customers** to maintain a **delivery address book** — full create / read / update / soft-retire — usable from account management and checkout, with:

1. **Customer soft-CRUD** for saved delivery addresses (default + optional labels).
2. **Active-order lock** — update or retire must fail while any **open** order references that address id.
3. **No hard deletes** — retire marks the row; history and audit remain.
4. **Edit = soft-retire + insert** — prior row is retired; a new row carries the updated values (and inherits default when the prior was default).
5. **Checkout UX** — when the book is non-empty, the customer picks a saved address; when empty, they must supply an address and the platform **saves it to the book** for reuse.
6. **Shop-admin read-only** view of a customer’s addresses.

Delivery documents remain **free-form JSON** ([ADR-015](../adr/ADR-015%20Address%20Storage%20as%20JSON.md)). Billing (`/me/billing`) stays separate (WP4b / #29). Guests have **no** address book.

### Current behaviour (as-built)

| Surface | Today |
|---|---|
| Checkout shipping | `POST /checkout/{id}/shipping` with free-form `delivery_address` only (ADR-015) |
| Customer profile | `/me/billing` structured; **no** delivery address collection |
| Order | Snapshots `delivery_address` JSON; **no** `delivery_address_id` |
| Search / fit-gap | `addresses` → 0 (Unsupported) |
| Soft-delete / lock | None for delivery addresses |

---

## Decisions (locked for this draft — 2026-10-28)

| Topic | Decision |
|---|---|
| Owning service | **yc-identity** owns the book. Checkout resolves ids and may create the first/new entry; orders store snapshot JSON **and** `delivery_address_id`. |
| Address document | Free-form JSON per ADR-015 (tenant frontend fields). Platform validates non-empty object + size only (same spirit as checkout `ValidateAddress`). |
| Guests | **No** address book. Guests continue to pass free-form `delivery_address` only. |
| Max active addresses | **5** per customer (non-deleted). Create / auto-save beyond that → **400**. |
| Edit semantics | Soft-retire current row (`deleted_at`); insert successor; set `replaced_by_id` on the retired row. |
| Default on succession | If the edited address **was** default, the **new** row **inherits** `is_default`. |
| Active-order lock | Use existing `Status.Open()`: `pending_payment`, `received`, `packaged`, `shipped`, `delivery_exception`. |
| Order binding | Store **`delivery_address_id`** on the order (plus full JSON snapshot). Lock queries by that id. |
| Checkout — book non-empty | Customer **selects** a book entry (`delivery_address_id`). May also add a new address (JSON) while under the max of 5; that creates a book row then binds it. |
| Checkout — book empty | Customer **must** supply `delivery_address` JSON; platform **creates** a book entry and binds `delivery_address_id` (first entry becomes default). |
| Shop admin | **Read-only** list/get of a customer’s addresses (active by default). No admin create/update/retire in this package. |
| GDPR | Book rows cleared/anonymised with `identity.user.deleted.v1` like billing. Order snapshots follow ADR-005 order PII rules. |
| Events | No new public event required for MVP; book is sync CRUD. |
| ADR / docs | ADR-015 short note (book metadata around free-form JSON); service docs; OpenAPI (ADR-028). |

---

## Part A — Address book (yc-identity)

### Storage

Table e.g. `customer_delivery_addresses` (tenant RLS):

| Field | Notes |
|---|---|
| `id` | UUID PK |
| `tenant_id`, `user_id` | Owner |
| `label` | Optional (e.g. “Home”, “Office”); max length TBD in OpenAPI (~64) |
| `address` | JSONB (ADR-015) |
| `is_default` | At most one **active** default per user |
| `deleted_at` | Null = active; set on retire / succession |
| `replaced_by_id` | Optional FK to successor after edit |
| `created_at`, `updated_at` | |

**Constraints**

- Count of rows with `deleted_at IS NULL` ≤ **5** per `(tenant_id, user_id)`.
- At most one active row with `is_default = true`.
- Setting a new default clears the previous active default.

### Customer API

| Method | Path | Auth | Effect |
|---|---|---|---|
| GET | `/me/addresses` | customer | List **active** addresses (newest / default-first — document sort in OpenAPI). Optional `?include_deleted=true` for the owner’s audit view. |
| POST | `/me/addresses` | customer | Create; body: `address` (required), `label?`, `is_default?`. **400** if already 5 active. First address forces `is_default`. |
| GET | `/me/addresses/{id}` | customer | 200 if owned (including soft-deleted); **404** otherwise. |
| PUT | `/me/addresses/{id}` | customer | **Succession edit.** If locked → **409**. Else soft-retire `{id}`, insert successor with new `address` / `label` / default rules; response is the **new** resource. |
| DELETE | `/me/addresses/{id}` | customer | Soft-retire. If locked → **409**. If it was default, promote another active address to default if any remain (deterministic: earliest `created_at`). |

Visitors / guests: **403** on `/me/addresses*`.

### Shop-admin API (read-only)

| Method | Path | Auth | Effect |
|---|---|---|---|
| GET | `/admin/users/{user_id}/addresses` | shop_admin | Tenant-scoped; list that customer’s addresses (default **active only**; optional `include_deleted`). |
| GET | `/admin/users/{user_id}/addresses/{id}` | shop_admin | Read one; **404** if wrong tenant / missing. |

No admin POST/PUT/DELETE for addresses in this package.

### Internal API

| Method | Path | Caller | Effect |
|---|---|---|---|
| GET | `/internal/users/{user_id}/addresses/{id}` | checkout (service token) | Active address JSON + metadata for resolution; **404** if missing/deleted/wrong user. |
| POST | `/internal/users/{user_id}/addresses` | checkout | Create from checkout auto-save / “new address” path (same validation + max-5). |

### Lock check (identity → orders)

Before PUT succession or DELETE soft-retire, identity calls yc-orders:

`GET /internal/orders/address-in-use?address_id={id}` → `{ "in_use": true|false, "order_ids"?: [...] }`

`in_use` is true iff any order for that tenant has `delivery_address_id = id` **and** `status` ∈ `Status.Open()`.

On `in_use`: return **409** Problem (`delivery_address_id` / `address_in_use`) so the BFF can show a clear message.

---

## Part B — Orders (yc-orders)

### Snapshot

On create (from checkout), persist:

- `delivery_address` — full JSON (unchanged ADR-015 behaviour).
- `delivery_address_id` — optional UUID; set for **customer** checkouts that used the book (including auto-save). Guests: null.

Expose both on customer/admin/internal order payloads.

### Internal lock endpoint

| Method | Path | Effect |
|---|---|---|
| GET | `/internal/orders/address-in-use?address_id=` | As above; uses `Status.Open()`. |

Fulfilment never live-joins the book; retired book rows must not break shipped orders.

---

## Part C — Checkout (yc-checkout)

### Shipping step

Extend `POST /checkout/{id}/shipping` (physical items only):

| Caller | Input | Behaviour |
|---|---|---|
| Guest / visitor | `delivery_address` JSON | Unchanged; no book. |
| Customer, book **empty** | `delivery_address` JSON **required** | Quote as today; **create** book entry via identity internal POST; set session `delivery_address` + `delivery_address_id`; first entry is default. |
| Customer, book **non-empty** | `delivery_address_id` | Resolve via identity internal GET; quote resolved JSON; bind id + JSON on session. |
| Customer, book **non-empty** | `delivery_address` JSON (new place) | If under max 5: create book entry, then bind. If at max: **400** (must pick an existing id or retire one in account UI first). |

Sending both id and JSON: **400** (ambiguous). Digital-only sessions: still no delivery address.

Changing address (new id or new JSON) clears any bound delivery **slot** (WP4c rule).

### Confirm / order create

Pass through snapshotted `delivery_address` and `delivery_address_id` to yc-orders. Confirm must not accept a different address without redoing the shipping step (existing rule).

### Session fields

Persist `delivery_address_id` on the session alongside `delivery_address` so abandon/retry and order create stay consistent.

---

## Default and label rules (summary)

| Event | Default | Label |
|---|---|---|
| First active address (create or checkout auto-save) | Becomes default | Optional; may be empty |
| Create with `is_default: true` | Clears prior default | As supplied |
| Succession edit of default | New row is default | Copied unless body overrides |
| Succession edit of non-default | New row not default unless body sets it | Same |
| Soft-retire of default | Promote oldest remaining active, if any | — |
| Soft-retire of non-default | Unchanged | — |

---

## GDPR / deletion

On customer delete (`DELETE /me` / admin delete):

- Soft-deleted and active book rows are removed or anonymised with the user (same class as billing).
- Orders keep historical `delivery_address` / `delivery_address_id` under ADR-005 retention / pseudonymisation (id may dangle; snapshot JSON is authoritative for fulfilment).

Account deletion already blocks on open orders (ADR-005 / existing identity→orders check); that remains the gate for deleting the user, distinct from per-address lock.

---

## ADR / docs impact

1. **ADR-015** — short amendment/note: delivery stays free-form JSON; identity may store labelled book rows around those documents; billing remains the structured exception (WP4b).
2. Service docs: yc-identity, yc-checkout, yc-orders.
3. OpenAPI + release notes (ADR-028); MCP if applicable.
4. Fit-gap: GAP-007 / GAP-008 / CUST-005 / CUST-006 → supported once shipped.

---

## Acceptance criteria

- [ ] Customer OpenAPI for `/me/addresses` soft-CRUD; list defaults to non-deleted; optional include-deleted.
- [ ] Edit = soft-retire + insert with `replaced_by_id`; default inherits when prior was default.
- [ ] Max **5** active addresses; create beyond that → 400.
- [ ] Update/retire while any `Status.Open()` order has that `delivery_address_id` → **409**.
- [ ] Guests cannot use the book; customer empty-book checkout requires JSON and saves to the book; non-empty allows pick-by-id (and new JSON under max).
- [ ] Order stores immutable `delivery_address` JSON **and** `delivery_address_id`; fulfilment unaffected by later retire.
- [ ] Shop admin read-only `GET /admin/users/{id}/addresses` (+ get by id).
- [ ] GDPR cascade covers book rows; ADR-015/service docs/OpenAPI updated.

---

## Out of scope

- Billing profile (`/me/billing`) changes.
- Hard delete of address rows outside GDPR account deletion.
- Multi-destination / split shipment per order.
- Admin create/update/retire of customer addresses.
- General customer `PATCH /me` (API-Q004).
- Guests gaining an address book.
- Changing lock definition away from `Status.Open()`.

---

## Test plan

1. Identity: create up to 5 → 6th 400; GET list hides deleted; PUT succession retires old + returns new; default inherits on edit of default.
2. Lock: place order with `delivery_address_id`; PUT/DELETE that id → 409 while `received` (etc.); after `delivered`, retire succeeds.
3. Checkout guest: JSON only; no identity address rows.
4. Checkout customer empty book: JSON → book row + session id; order has both snapshot and id.
5. Checkout customer with book: id resolves and quotes; JSON new under max creates; at max JSON → 400.
6. Admin: shop admin lists customer addresses; other tenant → 404; no write routes.
7. GDPR: delete user clears book; historical order snapshot still readable per existing rules.

---

## Implementation sketch

1. Spec → **Approved** (this draft).
2. yc-orders: `delivery_address_id` column + `GET /internal/orders/address-in-use` + OpenAPI.
3. yc-identity: migration + domain + `/me/addresses*` + admin read + internal get/create + lock call + OpenAPI.
4. yc-checkout: resolve/auto-save on shipping; session + order create pass-through; OpenAPI.
5. Docs (ADR-015 note, service docs); tests/lint; tag per ADR-028 / ADR-031 when ready to deploy.

---

## Review checklist (for approver)

- [ ] Confirm max **5** and succession default inheritance.
- [ ] Confirm checkout empty-book **must** auto-save (no “use once without saving”).
- [ ] Confirm non-empty path may still **add** a new book address via JSON when under max (as specified above).
- [ ] Confirm shop-admin surface is list/get only under `/admin/users/{id}/addresses`.
