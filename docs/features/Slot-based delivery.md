# Slot-based delivery

## Overview
**Status:** Shipped (`v1.1.0-beta.3`, issue [#30](https://github.com/slaghuis/yc-platform/issues/30) closed)
**GitHub Issues:** [#30](https://github.com/slaghuis/yc-platform/issues/30)
**Priority:** High (Wisefood checkout §7.2; fit-gap GAP-005 / CHK-004 / CHK-005)
**Repository:** yc-platform — primarily **yc-shipping** + **yc-checkout** + **yc-orders**; yc-message for template context. Storefront/BFF consumes OpenAPI (ADR-028).
**Work package:** WP4c (after WP4b / #29)

## Requirement

Enable shops to offer **concrete delivery date/time windows** at checkout — not only a shipping method with `estimated_days_min` / `estimated_days_max` — with:

1. **Shop admin configuration** of slot calendars (window length, open hours, capacity, lead time, cut-off, blackouts, timezone).
2. **Checkout APIs** to list available slots for the chosen shipping option and bind exactly one slot before pay.
3. **Capacity** so two customers cannot book the last remaining capacity for the same window.
4. **Immutable snapshots** on the session and placed order for fulfilment and messaging.

Slots **compose with** existing zone/rate quoting ([ADR-022](../adr/ADR-022%20Order%20Fulfilment%20Flow.md)); they do not replace pricing. Carrier live ETA APIs stay out of scope.

### Current behaviour (as-built)

| Surface | Today |
|---|---|
| Quote | `POST /checkout/{id}/shipping` → yc-shipping `POST /internal/quote` → zone match + priced rates, cheapest first (ADR-022 rule 1) |
| Option shape | `id` (method), `rate_id`, `name`, `amount_minor`, optional estimated days — **no start/end datetime** |
| Confirm | Requires shipping step for physical items; no slot field |
| Order snapshot | Delivery address + shipping option snapshot; no delivery window |
| Admin | Zones/rates/shipments CRUD; packaging books a label (ADR-022 rule 4) |
| Docs | yc-shipping “Open Questions” lists **Delivery scheduling / time slots** as future |
| Search | No platform `slot` APIs |

Free-text “preferred time” inside `delivery_address` is **not** a slot: it cannot reserve capacity or guarantee a window.

---

## Decisions (approved 2026-10-28)

| Topic | Decision |
|---|---|
| Owning service | **yc-shipping** owns calendars, capacity, and availability. Checkout is a thin client; orders store a snapshot only. |
| Compose with rates | Slots attach to a **rate**. A rate may set `requires_delivery_slot: true` and must reference a `schedule_id`. Rates without that flag behave as today. |
| When slots are required | Only when the **selected** rate requires them. Digital-only checkouts never see slots (ADR-010). |
| Calendar model | Admin **schedules** (rules): timezone (IANA), days of week, open/close, window length, capacity, lead time, cut-off, blackouts. Windows are **generated**; capacity from a bookings table. |
| Window identity | Stable id for `(schedule_id, start_at)` so list → select → book refer to the same window. |
| Checkout step | After shipping option selected: `GET /checkout/{id}/slots?from=&to=` (max **14 days**). `PUT /checkout/{id}/slot` selects one and **must verify remaining capacity**. Confirm validates the selection. |
| Soft select vs hard book | **Select = soft bind** on the session (capacity checked, not decremented). **Hard book at ORDER_CREATE** (atomic); **release on abandon** / compensate. Concurrent last-slot books: one wins, other **409**. |
| Stale slots | Confirm of a window that is past cut-off, blacked out, or full → **409**. Changing shipping address or option clears the bound slot. |
| Surcharge | **None in MVP.** |
| Order snapshot | `delivery_slot: { slot_id, start_at, end_at, timezone, label? }` on session and order. |
| Fulfilment | Admin views expose snapshot; packaging/carriers unchanged. |
| Admin reschedule | **Out of MVP** — future release. |
| Messaging | Optional template context keys; **`order.placed.v1` unchanged**. |
| Timezone | IANA on schedule; default **`Africa/Johannesburg`** when omitted (SA MVP). API returns UTC instants + timezone + optional local label. |
| Rate validation | `requires_delivery_slot: true` without a valid `schedule_id` → **hard 400** on rate create/PUT. |
| List range | Max **14 days** per request; default `from` = today (shop TZ). |
| Events | Snapshot on order row only; message enrichment as today — no event schema change. |
| ADR impact | ADR-022 amendment; yc-shipping.md; OpenAPI (ADR-028). |

---

## Part A — Admin: schedules and rate flag (#30)

### Schedule (yc-shipping)

Shop-admin CRUD, tenant-scoped:

| Field | Notes |
|---|---|
| `name` | Display (e.g. “Standard delivery windows”) |
| `timezone` | IANA; required |
| `window_minutes` | e.g. 30; fixed length per schedule in MVP |
| `weekdays` | Which days generate windows (Mon–Sun) |
| `open_time` / `close_time` | Local wall times; windows tile from open to close |
| `capacity_per_window` | Integer ≥ 1 |
| `min_lead_minutes` | Earliest selectable start = now + lead (in shop TZ) |
| `same_day_cutoff` | Optional local time; after cut-off, today’s remaining windows disappear |
| `blackout_dates` | List of local dates with no windows |
| `active` | Inactive schedule generates no availability |

**Binding to rates:** each rate may reference `schedule_id` and set `requires_delivery_slot`. One schedule may be shared by several rates. Zone matching stays on the rate via existing quote; slots are only offered for the selected rate’s schedule.

### API (sketch)

| Method | Path | Auth | Effect |
|---|---|---|---|
| GET/POST | `/admin/shipping/schedules` | shop_admin | List / create |
| GET/PUT/DELETE | `/admin/shipping/schedules/{id}` | shop_admin | Read / replace / delete (409 if rates still reference) |
| PUT | `/admin/rates/{id}` | shop_admin | Set `schedule_id`, `requires_delivery_slot` (extend existing rate PUT) |

No public catalogue of slots without a checkout session (avoids scraping capacity).

---

## Part B — Availability and booking (yc-shipping internal)

### List available

`GET /internal/slots` (service) or checkout-facing wrapper:

Query: `rate_id`, `from` (date), `to` (date), optional `limit`.

Behaviour:

1. Load rate → schedule (404/empty if rate has no schedule).
2. Generate candidate windows in range in the schedule timezone.
3. Apply lead time, cut-off, blackouts.
4. Subtract bookings (`capacity_per_window - booked_count`); drop full windows.
5. Return `{ slots: [{ slot_id, start_at, end_at, timezone, remaining_capacity }] }` sorted by `start_at`.

Cap the range (e.g. max 14 days per request) so generation stays cheap.

### Book / release

| Call | When | Effect |
|---|---|---|
| `POST /internal/slots/{slot_id}/book` | Checkout **ORDER_CREATE** (idempotent key = `checkout_session_id`) | Increment booked; 409 if full or invalid |
| `POST /internal/slots/{slot_id}/release` | Abandon / compensate after a successful book | Decrement if this key holds the booking |

**Booking moment (approved):** book at **ORDER_CREATE**, release on abandon/compensate. Confirm does not re-book.

Idempotency: repeating book with the same key returns the same booking.

---

## Part C — Checkout (#30)

### Flow

1. `POST /checkout/{id}/shipping` — unchanged quote + select option.
2. If selected option’s rate `requires_delivery_slot`:
   - `GET /checkout/{id}/slots?from=&to=` → available windows (checkout calls yc-shipping internal).
   - `PUT /checkout/{id}/slot` `{ "slot_id": "…" }` → bind on session (validate still available; do **not** hard-book yet).
3. Confirm:
   - If rate requires slot and none bound → **400** field `delivery_slot`.
   - If bound slot no longer valid → **409**; client re-lists.
4. Digital-only / rate without slots: slot endpoints **400** or return empty with no requirement.

### Session / confirm snapshot

```json
"delivery_slot": {
  "slot_id": "…",
  "start_at": "2026-11-03T08:00:00Z",
  "end_at": "2026-11-03T08:30:00Z",
  "timezone": "Africa/Johannesburg",
  "label": "3 Nov 2026, 10:00–10:30"
}
```

`label` is optional convenience for templates/UI; source of truth is `start_at`/`end_at`/`timezone`.

Pass through on `POST /internal/orders` create body; yc-orders persists JSONB.

---

## Part D — Orders, fulfilment, messaging

| Surface | Behaviour |
|---|---|
| `GET /orders/{id}`, admin detail | Include `delivery_slot` when present |
| Package / shipment | Unchanged; admin UI shows the window next to address |
| Cancel / abandon | Release booking in yc-shipping (orders or checkout compensation calls release) |
| GDPR | Slot times are not customer PII by themselves; retained with the order like other fulfilment facts |
| yc-message | Add optional context keys for order confirmation / shipped when slot present |

---

## ADR / docs impact

1. **ADR-022 amendment** — slot step, confirm validation, book/release relative to saga states.
2. **yc-shipping.md** — schedules, internal slots, remove from pure “future”.
3. **yc-checkout.md** / **yc-orders.md** — session field, APIs, snapshot.
4. **yc-message** — template context keys (and default template tweak if desired).
5. OpenAPI + release notes (ADR-028).

---

## Acceptance criteria

- [x] Shop admin can CRUD a delivery schedule and attach it to a rate with `requires_delivery_slot`.
- [x] Checkout lists only available windows for the selected rate (lead time, cut-off, blackouts, capacity).
- [x] Confirm without a required slot → 400; stale/full slot → 409; successful path snapshots the window on the order.
- [x] Two concurrent books of the last capacity: one succeeds, one 409; no oversell.
- [x] Abandon / compensate releases the booking so the window reappears.
- [x] Digital-only and non-slot rates unchanged (no breaking change for existing shops).
- [x] Order/admin payloads expose `delivery_slot`; confirmation email context can render it.
- [x] ADR-022 amendment + OpenAPI published; no carrier ETA dependency.

---

## Out of scope

- Slot-specific price surcharges.
- Admin reschedule / move booking after place.
- Multi-schedule per rate or customer “preferred recurring” slots.
- Carrier / driver consumption of slots.
- Pickup-point or in-store collection slots (can reuse model later).
- Replacing zone/rate pricing or estimated_days (those remain for non-slot rates).

---

## Test plan

1. Admin: create 30-minute weekday schedule, attach to standard rate; quote still returns amount; option/rate metadata shows slots required.
2. Checkout: list next 7 days; cut-off hides today’s remaining; blackout day empty; capacity 1 → second session cannot confirm the same window.
3. Saga: book at order create; abandon releases; successful place keeps snapshot after admin changes the schedule.
4. Digital cart: no slot routes required; confirm works.
5. Messaging: order confirmation context includes slot fields when snapshotted.
6. Auth: slots list requires session owner; admin schedules shop-scoped.

---

## Decisions locked (approval 2026-10-28)

1. **Book at ORDER_CREATE** + release on abandon/compensate.
2. **`PUT /slot` must verify remaining capacity** (still soft — no decrement until book).
3. **Default timezone `Africa/Johannesburg`** when omitted.
4. **`requires_delivery_slot` without schedule → hard 400** on rate create/PUT.
5. **Max 14 days** per list request; default `from` = today.
6. **No admin reschedule** in this package (future release).
7. **`order.placed.v1` unchanged.**

---

## Implementation sketch

1. Spec → Approved; ADR-022 amendment.
2. yc-shipping: schedules + admin API + internal list/book/release + rate fields.
3. yc-checkout: slots list/select + session + confirm + book at ORDER_CREATE + release on abandon.
4. yc-orders: `delivery_slot` JSONB + API exposure.
5. yc-message: optional template context keys.
6. Docs, OpenAPI, tests; storefront contract for Wisefood §7.2.
