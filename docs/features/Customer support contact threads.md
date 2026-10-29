# Customer support contact threads

## Overview
**Status:** Approved (implemented; awaiting release tag)
**GitHub Issues:** [#33](https://github.com/slaghuis/yc-platform/issues/33)
**Priority:** High (Wisefood **JRN-007** Must; fit-gap GAP-009 / CONT-001)
**Repository:** yc-platform — new service **yc-support**; **yc-message** (notifications); **yc-preference** (customer channel resolution); **yc-tenant** (`contact_email`); **yc-identity** (claim on register). Storefront/BFF consumes OpenAPI (ADR-028).
**Work package:** WP5 (after WP4d / #32)

## Requirement

Enable shops to run **Customer Support Contact** as a durable, referenced conversation:

1. Anyone (customer or visitor) can **submit** a support request and receive a **unique acknowledgement reference**.
2. Shop admins and customers exchange **messages on the same thread** (contact → response → reply → …) until **closed**.
3. **Notifications** go out on create, each counterpart message, auto-close, and manual close — customers via preferred channels ([ADR-023](../adr/ADR-023%20Customer%20Messaging.md) / yc-preference); shop via tenant **`contact_email`** ([ADR-029](../adr/ADR-029%20Tenant%20Registry.md)); visitors via the email on the request only.
4. **Authenticated customers** manage threads in the storefront account area; **visitors** participate by **email only** (plus a shop-admin record). Visitors may **claim** prior threads after registration when the request email matches.

### Current behaviour (as-built)

| Surface | Today |
|---|---|
| Support / ticket APIs | **yc-support** (OpenAPI; create/list/reply/admin/close/claim) |
| Contact | Threaded requests with unique `SUP-…` reference |
| Messaging | yc-message types `support_*`; sync `/internal/send` |
| Tenant | Shop notified at `contact_email` |
| Claim | Identity calls claim after password/OAuth register |

---

## Decisions (approved 2026-10-28)

| Topic | Decision |
|---|---|
| Owning service | **New `yc-support`** service (own DB, RLS, outbox, OpenAPI). |
| Status model | `new` / `open_customer` / `open_shop` / `closed`. **No reopen after `closed`.** |
| Close actors | Shop admin Close + **72h idle** auto-close only (no customer Close). |
| Auto-close | Measured from **`last_activity_at`** (any message). Notify customer/visitor + shop. |
| Visitor identity | Every request stores a normalised **contact email**. Visitors have no storefront thread UI. |
| Visitor participation | **Option A:** notify by email; **in-app only after claim**. No inbound email→thread in MVP. |
| Claim | After registration with the **same email**, threads are linked to the customer. |
| Attachments | **Phase 2** — text-only MVP. |
| Shop notifications | Always to the tenant’s **`contact_email`** (from yc-tenant). |
| Customer storefront | **`/account/support/{request_id}`** (authenticated customers only). |
| Visitors | Email notifications + shop-admin UI only. |
| Staff authors | **`shop_admin` only** in MVP. |
| Messaging | yc-message templates + channel resolution (ADR-023); transactional. |
| Rate limits | Align with other public write APIs; shop may add CAPTCHA in BFF. |

---

## Status machine

```
                    shop responds
   new ──────────────────────────► open_customer
    │                                    │
    │ customer replies                   │ customer replies
    │ (after shop has written)           ▼
    │                              open_shop ◄──┐
    │                                    │      │ shop responds
    │                                    └──────┘
    │
    └── (any non-closed) ── 72h idle OR staff/customer close ──► closed
```

| Status | Meaning | Who acts next |
|---|---|---|
| `new` | Request created; no shop message yet | Shop |
| `open_customer` | Shop has responded; waiting on the requester | Customer / visitor (email) |
| `open_shop` | Requester has replied; waiting on the shop | Shop |
| `closed` | Terminal — no further messages; **no reopen** | — |

**Transitions**

| From | Event | To |
|---|---|---|
| `new` | Shop posts first response | `open_customer` |
| `open_customer` | Customer/visitor posts reply | `open_shop` |
| `open_shop` | Shop posts response | `open_customer` |
| `new` / `open_*` | Staff **Close** | `closed` |
| `new` / `open_*` | **72h** since last message (`last_activity_at`) | `closed` (system) |
| `closed` | Any post | **409** |

Creating the request sets `new` and `last_activity_at = now`. Each message updates `last_activity_at`. Auto-close is a sweeper in yc-support (same lease/sweeper spirit as checkout).

---

## Part A — Domain (yc-support)

### Request (thread header)

| Field | Notes |
|---|---|
| `id` | UUID |
| `tenant_id` | RLS |
| `reference` | Unique per shop, human-facing (e.g. `SUP-…` / opaque short code) |
| `topic` | Code from catalogue (allergen, delivery, order, payment, feedback, other, …) |
| `subject` / first-line | Optional short subject; body lives in first message |
| `status` | `new` \| `open_customer` \| `open_shop` \| `closed` |
| `contact_email` | Required, normalised; join key for visitors + claim |
| `contact_name` | Required |
| `contact_phone` | Optional E.164 |
| `user_id` | Nullable until claimed / customer-created |
| `user_type` | `customer` \| `visitor` at create; claim sets customer |
| `order_id` | Optional context |
| `page_url` | Optional |
| `last_activity_at` | For 72h idle |
| `closed_at`, `closed_reason` | `staff` \| `idle` \| `customer` (if customers may close — **propose: shop or idle only in MVP**; customer cannot close) |
| `created_at`, `updated_at` | |

**Locked proposal for close actors:** shop_admin Close + system idle auto-close. Customers/visitors do not close (they stop writing; idle closes). Call out in Review checklist if product wants customer Close.

### Message

| Field | Notes |
|---|---|
| `id` | UUID |
| `request_id` | Parent |
| `author_type` | `customer` \| `visitor` \| `shop_admin` \| `system` |
| `author_user_id` | When known |
| `body` | Plain text (MVP); max length in OpenAPI |
| `created_at` | Immutable; no edit/delete in MVP |

First customer/visitor message is created with the request in one transaction.

### Topic catalogue

Platform-seeded codes per tenant (or global enum in OpenAPI for MVP):

`allergen` | `delivery` | `order` | `payment` | `feedback` | `other`

Shop-configurable labels/routing beyond `contact_email` → Phase 2. MVP: topic is metadata for filter + templates.

---

## Part B — APIs

### Public / customer

| Method | Path | Auth | Effect |
|---|---|---|---|
| POST | `/support/requests` | visitor or customer | Create request + first message; return `reference`, `id`, `status=new` |
| GET | `/support/requests` | **customer** | List mine (by `user_id`, including claimed) |
| GET | `/support/requests/{id}` | **customer** | Get header + messages (owner only) |
| POST | `/support/requests/{id}/messages` | **customer** | Reply if not `closed` and owned |

Visitors **cannot** call list/get/reply over HTTP after create — follow-ups are by **email** into the platform (see Part D) or they register and claim. Shop always sees the thread in admin.

### Shop admin

| Method | Path | Auth | Effect |
|---|---|---|---|
| GET | `/admin/support/requests` | shop_admin | List/filter by status, topic, email, reference |
| GET | `/admin/support/requests/{id}` | shop_admin | Full thread |
| POST | `/admin/support/requests/{id}/messages` | shop_admin | Response; advances status |
| POST | `/admin/support/requests/{id}/close` | shop_admin | → `closed` (`closed_reason=staff`) |

### Internal

| Method | Path | Caller | Effect |
|---|---|---|---|
| POST | `/internal/support/requests/claim` | identity (service) | `{ user_id, email }` → attach matching unclaimed rows |
| GET | `/internal/support/requests/{id}` | message | Enrich notification context if needed |

### Claim (identity → support)

On successful **register** / **OAuth register** / first login after register when email is verified: yc-identity (or a subscriber on `identity.user.registered.v1`) calls claim with the new `user_id` and email. Matching `contact_email` rows with null/visitor `user_id` get `user_id` set. Already-closed threads are claimed too (history under “my requests”, read-only).

---

## Part C — Notifications (yc-message + preferences)

### Channel rules

| Audience | Channel |
|---|---|
| Authenticated **customer** | ADR-023 resolve via yc-preference (primary verified + driver → else account email → else SMS) |
| **Visitor** | Always the request’s `contact_email` (email driver). No preference profile. |
| **Shop** | Tenant `contact_email` from yc-tenant (internal GET / projection). |

Transactional: **not** gated by marketing/newsletter flags.

### Events → message types (propose)

Publish domain events from yc-support outbox; yc-message templates subscribe (ADR-023 style):

| Event | Customer/visitor notify | Shop (`contact_email`) notify |
|---|---|---|
| `support.request.created.v1` | Ack + reference (+ storefront link if customer) | New request summary |
| `support.message.posted.v1` (author shop) | New response (+ storefront link if customer) | — |
| `support.message.posted.v1` (author customer/visitor) | — | New reply summary |
| `support.request.closed.v1` (staff or idle) | Closed notice (idle: explain 72h inactivity) | Closed notice |

Idempotent `reference` per message send (e.g. `event:<id>:<type>`).

### Storefront deep link (customers only)

Propose ADR-027 path:

`/account/support/{request_id}`

- Included in **customer** emails only.
- Requires customer session; opens topic + full discussion.
- Visitors’ emails carry **no** storefront thread URL — body text + reference only (reply-by-email workflow).

Exact path locked with ADR-027 amendment when Approved.

---

## Part D — Visitor participation (Option A — approved)

Visitors have no authenticated reply API and **no inbound email→thread** ingestion in MVP.

- Visitor **creates** once via `POST /support/requests`.
- Shop **responses** are stored on the thread and **emailed** to `contact_email` (read-only for the visitor until claim).
- Visitor **register/claim** (same email) unlocks customer list/get/reply in the storefront.
- Inbound email parsing → Phase 2 if needed.

---

## Part E — Abuse, GDPR, ops

- **Rate limit** create (and customer reply) per IP / actor; Problem `429`.
- **GDPR:** on `identity.user.deleted.v1`, pseudonymise `user_id`, redact `contact_*` and message bodies per ADR-005 participant rules; yc-support reports cascade completed.
- **Retention:** closed threads retained per shop policy (default align with message/order operational windows); document in service README.
- **Service secrets:** yc-support as ADR-021 client of yc-message, yc-tenant, yc-preference (if sync send) / events for message; identity calls claim.

---

## Acceptance criteria

- [x] `yc-support` service with OpenAPI: create/list/get/reply (customer); admin list/get/respond/close; claim internal.
- [x] Unique `reference` on create; status machine as locked; **409** on post/close when `closed`; **no reopen**.
- [x] 72h idle sweeper auto-closes and notifies.
- [x] Visitors create with email; no customer list/get until claim; claim on register by email.
- [x] Customer emails include `/account/support/{id}` (or agreed ADR-027 path); visitor emails do not.
- [x] Shop notified at tenant `contact_email`; customers via preference resolution (ADR-023).
- [x] Staff messages: `shop_admin` only.
- [x] Attachments deferred (Phase 2).
- [x] GDPR cascade + docs; OpenAPI/MCP (ADR-028).

---

## Out of scope (MVP)

- Attachments / malware scanning (Phase 2).
- Extra staff roles beyond `shop_admin` (Phase 2).
- Inbound email → thread parsing (unless Review chooses option B).
- Live chat / WebSockets.
- Per-topic routing addresses (beyond topic metadata + single `contact_email`).
- Customer-initiated Close (unless Review adds it).
- Reopening closed threads.

---

## Test plan

1. Customer create → `new`, reference, ack notification, shop `contact_email` notified.
2. Shop reply → `open_customer`; customer notified on preferred channel; deep link present.
3. Customer reply → `open_shop`; shop notified; customer reply API 409 when `closed`.
4. Idle: freeze clock / set `last_activity_at` −73h → sweeper closes + idle notifications.
5. Visitor create → email ack without storefront link; admin sees thread; GET as visitor 401/403.
6. Register same email → claim → appears in `GET /support/requests`; can reply.
7. Closed thread: shop/customer post → 409; close again → 409 or idempotent 200 (document).
8. Non-owner customer GET → 404; other shop admin → 404.

---

## Implementation sketch

1. Spec → **Approved**; thin ADR for yc-support (or amendment listing new service + events); ADR-027 path row; ADR-023 message types.
2. Scaffold `yc-support` (db, RLS, outbox, gateway registration, service secret).
3. Domain + migrations + public/admin/internal APIs + OpenAPI.
4. Sweeper for 72h idle close.
5. Events + yc-message templates; tenant `contact_email` lookup; preference resolution for customers.
6. Identity claim hook on register.
7. Service docs + fit-gap GAP-009; tests/lint; release tag.

---

## Review checklist (approved 2026-10-28)

- [x] Status names `new` / `open_customer` / `open_shop` / `closed`.
- [x] No customer Close (shop + idle only).
- [x] Visitor participation **option A** (notify by email; in-app only after claim).
- [x] Storefront path `/account/support/{request_id}`.
- [x] 72h measured from `last_activity_at` (any message).
