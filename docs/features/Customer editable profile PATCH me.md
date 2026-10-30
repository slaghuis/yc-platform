# Customer editable profile (`PATCH /me`)

## Overview
**Status:** Approved (implemented)
**GitHub Issues:** [#37](https://github.com/slaghuis/yc-platform/issues/37)
**Priority:** High (Wisefood Manage Account / CUST-005; fit-gap GAP-007 remaining slice; shop OQ-008 closed for *display*; platform API-Q004)
**Repository:** yc-platform — **yc-identity** only. Storefront/BFF consumes OpenAPI (ADR-028); shop must not invent a local profile store.
**Work package:** WP4e (after WP3 / #27 OAuth profile fields; address book #32 is separate and shipped)

## Requirement

Add a **customer-facing profile mutation API** so authenticated users can update their own identity profile fields — display name, mobile phone, and profile picture — not only read them when OAuth happens to supply them.

Password-registered customers must be able to set phone and avatar without Google/Facebook. OAuth customers must be able to edit (and clear) values that were previously provider-sourced. Email and password stay out of this package.

Closes the remaining platform half of Manage Account profile editing for Wisefood: addresses (#32) and billing (`/me/billing`) already exist; this FR covers the `User` identity fields on `/me`.

### Current behaviour (as-built)

| Surface | Today |
|---|---|
| `GET /me` | Returns `User` with optional `display_name`, `phone_e164`, `profile_image_url` |
| `DELETE /me` | GDPR account deletion |
| Profile mutation | **None** — OpenAPI still says “no customer PATCH /me in this release” |
| Password register | May set `display_name`; phone and avatar stay empty |
| OAuth complete (#27) | Phone: set only when empty; image: refresh when provider sends one; omitted claims do not clear |
| Billing / addresses | Separate resources under `/me/billing` and `/me/addresses` |
| Preference / marketing | `yc-preference` (`PATCH /preferences/me`) — not this FR |

---

## Decisions (locked for this draft — 2026-10-30)

| Topic | Decision |
|---|---|
| Endpoint | **`PATCH /me`** — partial update; omit unchanged fields. |
| Name model | Keep single **`display_name`** (max 100 runes). Do **not** add given/family name columns. |
| Phone model | Existing optional **`phone_e164`** (E.164). Same normalisation as #27 (`NormalisePhoneE164`). |
| Avatar model | Keep **`profile_image_url`** as an **https URL** (max 2048). **Not** a `yc-image` id; no download into object storage in this package. |
| Clear semantics | Explicit JSON **`null`** clears an optional field (`phone_e164`, `profile_image_url`). Omitted = leave unchanged. Empty string for those fields → **400** (use `null` to clear). |
| Display name clear | `display_name: null` or `""` both clear to unset/empty (stored as NULL / empty); response omits or returns empty per existing `User` omitempty rules — document one: **omit when empty**. |
| Auth | Caller may mutate **only their own** `User`. Allowed actor types: **`customer`**, **`shop_admin`**, **`system_admin`** (own account). **Visitors → 403**. No admin edit of another user’s profile in this package. |
| Email / password | **Out of scope.** Reject `email`, `password`, and unknown properties (`additionalProperties: false` → 400 Problem). |
| OAuth coexistence (amends #27 image rule) | After this FR: OAuth complete **fills phone and image only when the stored value is empty**. Customer `PATCH` values are therefore **not** overwritten on later OAuth login. Omitted provider claims still never clear stored values. |
| Events | **No** new bus event for MVP (sync CRUD, same stance as address book). |
| GDPR | Unchanged: anonymise already clears `display_name`, `phone_e164`, `profile_image_url`. |
| Preference channels | Do **not** auto-copy phone into yc-preference SMS/WhatsApp channels. |
| ADR / docs | OpenAPI + `docs/services/yc-identity.md`; amend #27 merge prose; ADR-028 note for shop sync. No new ADR required (extends existing identity user row). |

---

## API

### `PATCH /me`

| | |
|---|---|
| Auth | Signed-in non-visitor (customer / shop_admin / system_admin) |
| Body | `ProfilePatch` (below) |
| Success | **200** + full `User` (same shape as `GET /me`) |
| Errors | **400** validation / unknown fields; **401** unauthenticated; **403** visitor or wrong actor; **404** if user row missing (should not happen for valid token) |

```yaml
# Conceptual OpenAPI — implement in services/yc-identity/api/openapi.yaml
paths:
  /me:
    patch:
      operationId: patchMe
      summary: Update the caller's profile fields
      description: |
        Partial update of display_name, phone_e164, and profile_image_url.
        Omit a field to leave it unchanged; send null to clear phone or avatar.
        Does not change email or password. OAuth re-login fills phone/image
        only when the stored value is empty (customer edits are preserved).
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: '#/components/schemas/ProfilePatch' }
      responses:
        '200':
          description: Updated user.
          content:
            application/json:
              schema: { $ref: '#/components/schemas/User' }
        '400': { $ref: '#/components/responses/Problem' }
        '401': { $ref: '#/components/responses/Problem' }
        '403': { $ref: '#/components/responses/Problem' }
        '404': { $ref: '#/components/responses/Problem' }

components:
  schemas:
    ProfilePatch:
      type: object
      additionalProperties: false
      minProperties: 1
      properties:
        display_name:
          type: string
          maxLength: 100
          nullable: true
          description: |
            Trimmed display name (max 100). null or "" clears.
        phone_e164:
          type: string
          nullable: true
          description: |
            E.164 (+ and 7–15 digits). null clears. Empty string is 400.
        profile_image_url:
          type: string
          maxLength: 2048
          nullable: true
          description: |
            https URL only (no http, javascript:, data:). null clears.
            Empty string is 400. Not a yc-image id.
```

### `User` schema text updates

Replace the “OAuth-only / no PATCH” prose:

| Field | Updated description |
|---|---|
| `phone_e164` | Optional E.164. Set via OAuth (when empty) or `PATCH /me`. Absent when unset. |
| `profile_image_url` | Optional https avatar URL. Set via OAuth (when empty) or `PATCH /me`. Absent when unset. Not a yc-image id. |
| `display_name` | Optional. Set at register / OAuth / invitation accept / `PATCH /me`. |

### Unchanged

- `GET /me`, `DELETE /me`
- `/me/billing*`, `/me/addresses*`
- Admin promote/demote/suspend surfaces (no admin profile-edit of another user)

---

## Domain / persistence

No new columns. Reuse `users.display_name`, `users.phone_e164`, `users.profile_image_url`.

### Validation (on PATCH)

Reuse / extend existing helpers in `domain`:

| Field | Rules |
|---|---|
| `display_name` | Trim; max 100 runes; `null` / `""` → store cleared |
| `phone_e164` | `NormalisePhoneE164`; invalid non-empty → **400** field error (unlike OAuth, which *skips* invalid claims); `null` → clear; `""` → **400** |
| `profile_image_url` | `NormaliseProfileImageURL` (must start with `https://`, ≤ 2048); invalid → **400**; `null` → clear; `""` → **400** |
| Body | `additionalProperties: false`; at least one property; empty `{}` → **400** |

### OAuth merge (amended)

```text
MergeOAuthProfile:
  phone  ← provider phone only if stored phone is empty (unchanged from #27)
  image  ← provider image only if stored image is empty  (**was:** always refresh)
  omitted claims never clear stored values                 (unchanged)
```

Update unit tests that asserted “always refresh image on OAuth”.

### Write path

1. Auth middleware → actor must be non-visitor; `actor_id` = user id.
2. Load user in tenant TX; 404 if missing/deleted.
3. Apply patch fields; validate; persist `updated_at`.
4. Return mapped `User`.

No outbox event for MVP.

---

## Shop / BFF expectations (ADR-028)

After the platform tag ships:

1. Sync OpenAPI; Manage Account profile form binds to `PATCH /me` + `GET /me`.
2. Do **not** store name/phone/avatar in a shop-local DB.
3. Avatar UX may be “paste https URL” or a later shop upload that yields an https URL (e.g. via yc-image public URL) — platform accepts only the URL string in this package.
4. OQ-008 (display of OAuth fields) remains closed without this FR; this FR closes **edit** (CUST-005 / GAP-007 / API-Q004).

---

## Acceptance criteria

- [x] OpenAPI: `PATCH /me` with `ProfilePatch`, Problem codes `400` / `401` / `403` / `404`.
- [x] `User` schema / descriptions no longer claim “no customer PATCH /me in this release”.
- [x] Password-registered customer can set and later clear `phone_e164` and `profile_image_url`.
- [x] OAuth customer can edit display name, phone, and avatar; subsequent OAuth login does **not** overwrite non-empty phone or image.
- [x] OAuth still fills phone/image when those fields are empty.
- [x] Invalid phone / non-https avatar → 400; unknown body fields → 400; visitor → 403.
- [x] Email unchanged by PATCH; attempts to send `email` / `password` → 400.
- [x] GDPR delete still clears the three fields.
- [x] Service doc + #27 merge rule docs updated; shop can implement Manage Account without a local profile store.

---

## Out of scope

- Delivery address book (#32) and billing (`/me/billing`).
- Preference / marketing flags (`yc-preference`).
- Changing login email or password (reset flow already exists).
- Admin or shop-admin editing **another** user’s profile fields.
- Uploading/copying avatars into yc-image as a platform-owned blob in this package.
- Auto-sync of phone into preference messaging channels.
- Structured first/surname fields.
- New `identity.user.profile_updated.v1` (defer unless a consumer appears).
- Shop UI implementation (yc-platform-shop consumes the contract after the tag).

---

## Test plan

1. **Password user:** register → `PATCH /me` set all three → `GET /me` reflects them → patch one field only → others unchanged.
2. **Clear:** `phone_e164: null` and `profile_image_url: null` omit fields on next GET; `""` → 400.
3. **Validation:** bad E.164, `http://…`, `javascript:…`, `data:…`, oversized name/URL, `{}`, `{ "email": "…" }` → 400.
4. **Auth:** visitor token → 403; customer A cannot affect user B (only own `/me`).
5. **OAuth amend:** user with customer-set image; OAuth complete with different picture → image **unchanged**; user with empty image; OAuth with picture → image **set**.
6. **OAuth phone:** empty phone + provider phone → set; non-empty phone + different provider phone → unchanged.
7. **GDPR:** `DELETE /me` clears columns (existing anonymise path).
8. **Roles:** shop_admin and system_admin can PATCH their own `/me` (smoke).

---

## Implementation sketch

1. Spec → **Approved**.
2. yc-identity: amend `MergeOAuthProfile` (image fill-when-empty); `ProfilePatch` domain + `UpdateProfile` + `PATCH /me`; OpenAPI + regenerate — **done**.
3. Tests: domain merge + HTTP integration for patch/clear/validation — **done**.
4. Docs: `docs/services/yc-identity.md`, this FR → Implemented — **done**.
5. Tag release (e.g. next `v1.1.0-beta.*`) so GHCR + demo VPS can pick it up — pending.

---

## Review checklist (for approver)

- [x] Confirm **single `display_name`** (no first/surname split).
- [x] Confirm avatar stays **https URL**, not yc-image id, for this package.
- [x] Confirm OAuth image rule change: **fill when empty only** (stops overwriting customer edits).
- [x] Confirm **`null` clears** phone/avatar; empty string is 400.
- [x] Confirm **no** new bus event for MVP.
- [x] Confirm shop_admin / system_admin may PATCH **own** `/me` (not other users).
