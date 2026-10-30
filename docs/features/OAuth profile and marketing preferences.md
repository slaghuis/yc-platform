# OAuth profile fields and marketing preferences

## Overview
**Status:** Implemented
**GitHub Issues:** [#27](https://github.com/slaghuis/yc-platform/issues/27), [#28](https://github.com/slaghuis/yc-platform/issues/28)
**Priority:** High (Wisefood customer profile / consent)
**Repository:** yc-platform only. yc-identity (#27); yc-preference + yc-message (#28).
**Work package:** WP3 (customer profile & consent)

## Requirement

Two independent customer-facing gaps:

1. **OAuth profile enrichment (#27)** — when a customer signs in with Google or Facebook, persist phone and profile image when the provider supplies them, and expose them on `GET /me`.
2. **Marketing / newsletter preferences (#28)** — store explicit opt-in flags on the preference profile so storefronts do not invent a parallel consent store, and so yc-message can honour them for non-transactional mail.

They share a customer journey but different services. No customer `PATCH /me` in this package (shop matrix API-Q004 stays a follow-up).

### Decisions proposed for this draft

| Topic | Proposal |
|---|---|
| #27 phone storage | Optional E.164 `phone_e164` on the identity user; omitted from JSON when empty |
| #27 image storage | Optional `profile_image_url` (https URL from the provider). **Not** a yc-image id — no download/copy into object storage in this package |
| #27 write path | OAuth complete only (and document password registration leaves these empty). **No** `PATCH /me` |
| #27 merge rules | If the provider sends a value: set phone when the user has none, always refresh `profile_image_url`. If the provider omits a claim: **do not clear** an existing stored value |
| #27 name fields | Keep single `display_name`; do not add given/family name columns unless a provider forces it |
| #27 scopes | Document per provider what we request and what we actually get (phone often absent) |
| #28 preference keys | Three booleans on preferences: `newsletter`, `marketing`, `new_products` — default **false** (opt-in) |
| #28 per-channel marketing | **Out of scope** for this package (topic-level flags only; channel address/verify stays as today) |
| #28 registration | Initial values via **`PATCH /preferences/me` after register** (or OAuth). Do not extend identity register body |
| #28 transactional mail | Unchanged: password reset, channel verification, order lifecycle, welcome, cart_abandoned, review_prompt stay sendable without marketing opt-in (existing yc-message v1 stance) |
| #28 marketing send gate | yc-message refuses to enqueue a message type classified as **marketing** unless the matching flag is true; no marketing templates ship in this package beyond the gate and docs |
| #28 absence of opt-in | `false` / missing / defaults → do not market |
| #28 event | `preference.marketing.changed.v1` when any of the three flags change |

---

## Part A — OAuth phone and profile image (#27)

### Current behaviour (as-built)

- Google/Facebook OAuth return `OAuthProfile{Subject, Email, EmailVerified, Name}` only.
- `User` / `GET /me`: `user_id`, `tenant_id`, `email`, `display_name`, `role`, `status`, `created_at`.
- Messaging phone lives on yc-preference channels, separate from identity profile phone.

### Provider honesty

| Provider | Image | Phone | Notes |
|---|---|---|---|
| Google | Usually available (`picture` on userinfo) when profile scope is granted | Rarely available on standard OAuth userinfo; do **not** promise People API phone in this package | Request existing scopes + ensure profile/email; parse `picture` when present |
| Facebook | Usually available (`picture.data.url` or equivalent) | Typically **not** returned for ordinary apps | Request picture; document phone as unsupported unless the Graph response includes it |

Never invent values. Log only that a claim was absent (no raw tokens, no full profile dumps at info level).

### Domain / persistence

Extend the identity user row (migration):

- `phone_e164 TEXT NULL` — normalised E.164 when set; validate on write
- `profile_image_url TEXT NULL` — https URL, max length documented (e.g. 2048); reject non-https

GDPR: on `DELETE /me` / admin delete, clear these with the user record (existing anonymise/delete path).

### API

`GET /me` (and any admin user payloads that embed `User`) add:

| Field | Required | Notes |
|---|---|---|
| `phone_e164` | no | Absent when unset |
| `profile_image_url` | no | Absent when unset |

OAuth start/callback operation descriptions list requested scopes and mapped claims. `Session` from callback stays tokens + ids; clients call `GET /me` for the enriched profile.

### Out of scope (#27)

- Customer `PATCH /me` / profile editor API (API-Q004).
- Uploading the avatar into yc-image.
- Syncing OAuth phone into yc-preference SMS/WhatsApp channels automatically.
- Apple / other providers.

---

## Part B — Marketing preferences (#28)

### Current behaviour (as-built)

- `GET`/`PATCH /preferences/me`: `display_currency`, `locale`, `primary_channel`, channels.
- Service doc Future Scope already lists marketing consent flags.
- yc-message: transactional types only in v1; doc already distinguishes transactional vs marketing.

### Preference model

Add to the stored preferences (and tenant defaults schema where defaults are stored):

```json
{
  "newsletter": false,
  "marketing": false,
  "new_products": false
}
```

| Flag | Meaning |
|---|---|
| `newsletter` | Periodic newsletter / editorial mail |
| `marketing` | Promotional / campaign messages (general) |
| `new_products` | New-product announcements |

Defaults for new rows and for `stored: false` shop defaults: all **false**. Shop admin may set tenant defaults via existing defaults endpoints once OpenAPI includes the fields (same PUT defaults surface).

### API

Extend `Preferences` and `PreferencesPatch`:

- Read: all three booleans always present on `GET /preferences/me` and `GET /internal/preferences/{user_id}`.
- Patch: each field optional; omit = leave unchanged.

No new paths. No per-channel marketing matrix in this package.

### Events

| Event | When |
|---|---|
| `preference.marketing.changed.v1` | One or more of `newsletter` / `marketing` / `new_products` changed (`user_id`, new values, `changed_at`; previous values optional) |

### yc-message gate

1. Classify each message type as `transactional` or `marketing` in code/docs.
2. **Transactional** (unchanged send rules): `welcome`, `password_reset`, `channel_verification`, `order_confirmation`, `order_shipped`, `order_delivered`, `order_cancelled`, `order_refunded`, `cart_abandoned`, `review_prompt`, admin alerts.
3. **Marketing** (future templates / campaigns): require the corresponding flag (`newsletter` / `marketing` / `new_products` as mapped per template). If unset/false, skip enqueue (log at info, no error to the publisher) or document equivalent behaviour.
4. Delivery still requires a usable address; for SMS/WhatsApp, existing verified-channel rules apply **in addition** to marketing opt-in.
5. This package may ship **zero** new marketing templates; the gate and preference read path are the deliverable so storefronts can store consent ahead of campaigns.

### Registration flow

1. Customer registers or completes OAuth.
2. Storefront immediately `PATCH /preferences/me` with the consent checkboxes the user ticked on the form.
3. Identity register/OAuth bodies are **not** extended.

### GDPR

Marketing flags delete with preferences on `identity.user.deleted.v1` (existing cascade). Document that consent records are not retained after deletion in this design.

## Out of scope (package-wide)

- Full ESP/CRM sync.
- Campaign CMS / editorial content.
- Changing cart_abandoned or review_prompt into marketing-gated types (would be a product/compliance decision later).
- yc-platform-shop UI beyond consuming the new fields after ADR-028 sync.

## Follow-up

- yc-platform-shop: social sign-in display of phone/image; consent checkboxes → PATCH (CUST-004, CUST-007, OQ-008, OQ-009).
- yc-platform-admin: optional defaults editor for marketing flags.
- yc-platform-mcp: one OpenAPI sync covering identity + preference (+ message docs if exposed).
- Later: optional copy of phone into preference channels; per-channel marketing flags; first real marketing templates.
- Customer `PATCH /me` (API-Q004 / #37): see [Customer editable profile PATCH me](Customer%20editable%20profile%20PATCH%20me.md).

## Test plan

**yc-identity**

1. OAuth complete with a fake/provider fixture that includes picture → `GET /me` has `profile_image_url`.
2. Fixture without phone → field absent; subsequent login without phone does not clear a previously stored phone.
3. Invalid phone from fixture rejected or ignored per validation rules (document choice: reject complete vs skip claim — **propose skip invalid claim, keep prior**).
4. GDPR delete clears the new columns.

**yc-preference**

1. Defaults expose three `false` flags; PATCH sets/clears individually.
2. `preference.marketing.changed.v1` published only when a flag value changes.
3. Internal GET returns the flags for yc-message.

**yc-message**

1. Unit/integration: marketing-classified send path no-ops when flag false; transactional types still send.
2. With flag true and (if a test marketing type exists) send proceeds subject to channel rules.

## Acceptance

- [ ] After OAuth, `GET /me` can return `phone_e164` and/or `profile_image_url` when the provider supplied them.
- [ ] Missing/denied claims do not invent data and do not wipe existing values.
- [ ] Provider scope/claim behaviour is documented in OpenAPI / service doc.
- [ ] `GET`/`PATCH /preferences/me` expose `newsletter`, `marketing`, `new_products` (default false).
- [ ] yc-message has a documented transactional vs marketing split and a gate for marketing types.
- [ ] GDPR covers new identity fields and preference flags.
- [ ] OpenAPI + events + service docs updated; ADR-028 sync called out. No `PATCH /me` in this package.

## Open for review

1. Profile image as **URL** (not yc-image) — confirm.
2. Phone fill-once vs always overwrite when provider sends a new number — draft = fill when empty only.
3. Marketing flag set (`newsletter` / `marketing` / `new_products`) vs fewer/more keys (e.g. drop `new_products`, add `shopping_process`).
4. Keep `review_prompt` and `cart_abandoned` **transactional** — confirm.
5. No marketing templates in this package (gate only) — confirm.
6. Invalid OAuth phone claim: **skip** vs fail the login — draft = skip.
