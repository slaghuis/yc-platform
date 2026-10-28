# yc-preference — Service Requirement

## Overview

`yc-preference` maintains per-customer preferences: display currency, preferred messaging channel, preferred search facets, and the last-used facet state. It is a lightweight settings store. All preference data is PII in the sense that it is linked to a user — it participates in the GDPR cascade.

## Location in Monorepo

```
/services/yc-preference/
```

## Responsibilities

1. Store and retrieve a customer's display currency preference
2. Store and retrieve a customer's preferred messaging channel (and address/handle per channel)
3. Store a customer's preferred faceted search filters
4. Store a customer's last-used faceted search state (for session continuity)
5. Seed defaults for new customers from tenant-level defaults
6. Provide fast reads for yc-product, yc-pricing, yc-cart, yc-message
7. Participate in the GDPR cascade

## Multi-Tenancy

- Preferences are keyed by `(tenant_id, user_id)`
- A single email may have different preferences in different tenants
- Tenant-level default preferences are stored as a separate record with `user_id = NULL`

## Preferences Model

Each preference is a **typed, named key**. New preference types are added by schema evolution, not by code.

```json
{
  "tenant_id": "shop-abc",
  "user_id": "user-uuid",
  "preferences": {
    "display_currency": "USD",
    "messaging": {
      "primary_channel": "email",
      "channels": {
        "email": { "address": "user@example.com", "verified": true },
        "whatsapp": { "address": "+27831234567", "verified": false },
        "sms": { "address": "+27831234567", "verified": true }
      }
    },
    "search": {
      "preferred_facets": { "category": "electronics", "price_max": 5000 },
      "last_used_facets": { "category": "electronics", "price_max": 5000, "brand": "acme" }
    },
    "locale": "en-ZA"
  }
}
```

**v1 explicit preference keys**: `display_currency`, `messaging.primary_channel`, `messaging.channels.*`, `search.preferred_facets`, `search.last_used_facets`, `locale`.

Future keys can be added additively.

## Channel Verification

- When a messaging channel is added or changed, yc-message is responsible for sending a verification message
- yc-preference stores the `verified` flag per channel but does not perform verification itself
- yc-message calls back into yc-preference to mark a channel verified
- The channel is **not used as primary** until verified (fallback to email if primary unverified)

## API

### Public Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| GET | `/preferences/me` | Get my preferences | customer / shop_admin / system_admin |
| PATCH | `/preferences/me` | Update my preferences (partial) | customer / shop_admin / system_admin |
| GET | `/preferences/me/channels` | List messaging channels | customer |
| POST | `/preferences/me/channels` | Add a channel | customer |
| DELETE | `/preferences/me/channels/{channel}` | Remove a channel | customer |

### Admin Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| GET | `/admin/preferences/defaults` | Get tenant default preferences | shop_admin |
| PUT | `/admin/preferences/defaults` | Set tenant default preferences | shop_admin |

### Internal Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| GET | `/internal/preferences/{user_id}` | Get preferences for a user (used by yc-pricing, yc-message) | service |
| GET | `/internal/preferences/{user_id}/channel` | Get resolved messaging channel | service |
| POST | `/internal/preferences/{user_id}/channels/{channel}/verify` | Mark channel verified (called by yc-message) | service |

### Phase 3 Implementation Notes

The contract is `services/yc-preference/api/openapi.yaml`. Implemented: `GET`/`PATCH /preferences/me`, the channel endpoints and the admin defaults, for customers and shop admins of a shop (system admins on `_platform` have no shop preferences). Internal endpoints, verification, preferred facets, notification settings and the GDPR cascade come later.

- **Model:** display currency, locale (BCP 47), messaging channels (email, sms, whatsapp, telegram) and a primary channel that must be one of the user's channels. Addresses are normalised: emails lowercased, sms and whatsapp in E.164, Telegram as `@handle`. A new or changed address is unverified; the primary channel cannot be removed (409).
- **Defaults.** A provisioned shop's defaults are its store currency and `defaults.locale` (`platform.tenant.provisioned.v1`); shops without defaults use `defaults` from configuration. `GET /preferences/me` returns the defaults with `stored: false` until the user changes something.
- **Seeding.** `identity.user.registered.v1` stores the shop defaults for the new customer, with their email as the email channel and primary channel, and publishes `preference.currency.changed.v1` without a previous value so consumers (yc-cart) learn the display currency. Seeding is idempotent.
- **Events:** `preference.currency.changed.v1`, `preference.locale.changed.v1`, `preference.channel.changed.v1` (primary), `preference.channel.added.v1` and `preference.channel.removed.v1`, each carrying the previous value where there is one. Verification and facet events come with yc-message and search personalisation.

### Phase 7 Implementation Notes

Channel verification follows ADR-023 rule 5: yc-message sends and checks the codes; yc-preference stores the result. yc-message does not call yc-preference to mark a channel verified, so `POST /internal/preferences/{user_id}/channels/{channel}/verify` and `GET /internal/preferences/{user_id}/channel` are not implemented.

- **Endpoints:** `POST /preferences/me/channels/{channel}/verify` (`{"code"}`; a wrong code is 400, an expired or exhausted one 409) and `POST /preferences/me/channels/{channel}/resend` call yc-message's `/internal/channels/verify-confirm` and `verify-start` with the `internal_calls` client (`YC_PREFERENCE_SERVICE_SECRET`). Only email and SMS can be verified.
- **Internal:** `GET /internal/preferences/{user_id}` returns a user's stored preferences with each channel's address and `verified` flag, or 404 when none are stored; yc-message reads the recipient and locale from it. Registration stores them, so every customer has some.
- **Events:** `preference.channel.verified.v1` when a code is confirmed. `message.verification_confirmed.v1` is also applied (idempotently). `message.bounced.v1` unverifies the bounced address, and a bounced primary channel other than email falls back to email, publishing `preference.channel.changed.v1`.

### Marketing preferences (issue #28)

- **`newsletter`**, **`marketing`**, **`new_products`** booleans on preferences and shop defaults (default false / opt-in). Exposed on `GET`/`PATCH /preferences/me`, defaults admin PUT, and `GET /internal/preferences/{user_id}`.
- **`preference.marketing.changed.v1`** when any flag changes. yc-message gates future marketing message types on these flags; current templates stay transactional. See `docs/features/OAuth profile and marketing preferences.md`.

## Database Schema

Database: **`yc_preference`**

```sql
CREATE TABLE preferences (
    tenant_id       TEXT NOT NULL,
    user_id         TEXT NOT NULL,          -- 'default' for tenant defaults
    display_currency TEXT,
    primary_channel TEXT,
    locale          TEXT,
    search_preferred_facets JSONB NOT NULL DEFAULT '{}',
    search_last_used_facets JSONB NOT NULL DEFAULT '{}',
    extensions      JSONB NOT NULL DEFAULT '{}',   -- forward compat
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, user_id)
);

CREATE TABLE messaging_channels (
    tenant_id       TEXT NOT NULL,
    user_id         TEXT NOT NULL,
    channel         TEXT NOT NULL,          -- 'email','sms','whatsapp','telegram'
    address         TEXT NOT NULL,
    verified        BOOLEAN NOT NULL DEFAULT FALSE,
    verified_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, user_id, channel)
);

CREATE TABLE outbox ( ... );
CREATE TABLE processed_events ( ... );

ALTER TABLE preferences ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON preferences
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform');
-- similar on messaging_channels
```

## Published Events

| Event | When |
|---|---|
| `preference.currency.changed.v1` | Display currency changed |
| `preference.channel.added.v1` | New messaging channel added |
| `preference.channel.changed.v1` | Primary channel changed |
| `preference.channel.verified.v1` | A channel was verified |
| `preference.channel.removed.v1` | Channel removed |
| `preference.facet.updated.v1` | Preferred facets changed |
| `preference.locale.changed.v1` | Locale changed |

## Subscribed Events

| Event | Action |
|---|---|
| `identity.user.registered.v1` | Create preference row seeded with tenant defaults |
| `identity.user.deleted.v1` | Delete all preferences for the user; publish `platform.gdpr.cascade_completed.v1` |
| `platform.tenant.provisioned.v1` | Create default preference row for the tenant |
| `platform.tenant.deleted.v1` | Delete all tenant preferences |
| `message.bounced.v1` | Mark channel as unverified; demote from primary if applicable |

## Configuration

```yaml
yc-preference:
  listen: :8080
  database:
    url: postgres://yc_preference:...@...
  defaults:
    display_currency: ZAR           # platform-wide default
    primary_channel: email
    locale: en-ZA
  nats:
    url: nats://...
  log_level: info
```

## Dependencies on Other Services

- **None at startup**

## Called By

- `yc-pricing` (for display currency conversion)
- `yc-product` (for faceted search context)
- `yc-cart` (for pricing display)
- `yc-message` (for routing)
- `yc-checkout` (for messaging channel on confirmations)

## GDPR Behavior

- On `identity.user.deleted.v1`: all rows for `user_id` deleted; publish `platform.gdpr.cascade_completed.v1`

*Phase 8:* Implemented as described (preferences and messaging channels); the action is reported as `deleted`.

## Open Questions / Future Scope

- Per-channel / per-category marketing consent matrix (topic flags newsletter/marketing/new_products are implemented)
- Theme / UI preferences
- Multi-language catalog preferences
- Notification scheduling (quiet hours, timezone-aware)
