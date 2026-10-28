# yc-identity — Service Requirement

## Overview

`yc-identity` is the authentication and user identity service. It handles OAuth (Google, Facebook), username/password authentication, visitor token issuance, token refresh, and role management. It is the sole authority on user identity across the platform.

## Location in Monorepo

```
/services/yc-identity/
```

## Responsibilities

1. Register users via username/password
2. Authenticate via Google OAuth
3. Authenticate via Facebook OAuth
4. Issue visitor tokens to anonymous callers
5. Issue access tokens on successful authentication
6. Refresh access tokens
7. Promote users through the role hierarchy
8. Associate guest orders with newly registered customers (post-purchase registration flow)
9. Handle user deletion requests and initiate the GDPR cascade (ADR-005)
10. Store OAuth-linked identities (one user may have multiple OAuth providers)

## Role Hierarchy

Linear promotion only; demotion is possible but requires system admin action.

- **visitor** — anonymous, has only a visitor token
- **customer** — registered, can place orders
- **shop_admin** — can administer products/pricing/inventory for a specific tenant (scoped to one tenant)
- **system_admin** — platform-wide administrator; not scoped to a tenant

Only system admins can promote to shop_admin or system_admin. Shop admins can view but not promote users.

## Multi-Tenancy

- **Visitors, customers, and shop admins** are scoped to a tenant
- **System admins** are platform-wide (`tenant_id = "_platform"`)
- A single email may register against multiple tenants as **separate customer accounts** (each tenant has its own storefront experience)
- The combination `(tenant_id, email)` is unique; `email` alone is not

## Token Design

Access tokens are **JWTs** signed by yc-identity. Claims include:

```json
{
  "sub": "user-uuid",
  "tenant_id": "shop-abc",
  "role": "customer",
  "actor_type": "customer",
  "iat": 1234567890,
  "exp": 1234571490,
  "jti": "token-uuid"
}
```

- Access token TTL: 15 minutes (configurable per tenant)
- Refresh token TTL: 30 days (configurable per tenant)
- Visitor tokens are short-lived access tokens with `role: "visitor"`
- `jti` is logged in actor fields of events for audit traceability

## API

### Public Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| POST | `/visitor` | Issue visitor token for a tenant | none |
| POST | `/register` | Register with email+password | none or visitor token |
| POST | `/login` | Login with email+password | none |
| POST | `/oauth/google/callback` | OAuth callback | OAuth state |
| POST | `/oauth/facebook/callback` | OAuth callback | OAuth state |
| POST | `/refresh` | Refresh access token | refresh token |
| POST | `/logout` | Revoke tokens | access token |
| POST | `/password/reset-request` | Initiate password reset | none |
| POST | `/password/reset-confirm` | Complete password reset | reset token |
| DELETE | `/user/me` | Delete own account (GDPR) | access token |

### Admin Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| GET | `/admin/users` | List users (tenant-scoped for shop_admin) | shop_admin / system_admin |
| GET | `/admin/users/{id}` | Get user | shop_admin / system_admin |
| POST | `/admin/users/{id}/promote` | Promote role | system_admin |
| POST | `/admin/users/{id}/demote` | Demote role | system_admin |
| POST | `/admin/users/{id}/suspend` | Suspend user | shop_admin / system_admin |
| DELETE | `/admin/users/{id}` | Delete user (triggers GDPR cascade) | system_admin |

### Internal Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| POST | `/internal/validate-token` | Validate an access token (called by yc-gateway) | service |
| POST | `/internal/service-token` | Issue a service token to a service client (ADR-021) | client secret |
| GET | `/internal/users/{id}` | Fetch minimal user info | service |

OpenAPI spec stub lives at `/services/yc-identity/api/openapi.yaml`.

### Phase 2 Implementation Notes

The contract is `services/yc-identity/api/openapi.yaml`. Phase 2 implemented `/visitor` (through the gateway), `/register`, `/login`, `/me`, `GET /admin/users`, `GET /admin/users/{id}`, `POST /admin/users/{id}/promote` and `/internal/validate-token`. Later phases added `/refresh`, `/logout`, `/password/reset-request` and `/reset-confirm`, `/oauth/{provider}/start` and `/callback`, `DELETE /me`, `DELETE /admin/users/{id}` (ADR-025) and `/internal/service-token` (ADR-021). Phase 12 (ADR-030) adds `/sysadmin/invitations`, anonymous `POST /invitations/accept`, and `POST /admin/users/{id}/suspend|reactivate|demote`.

- **Passwords** are hashed with Argon2id (`password.argon2`: time 2, 64 MiB, 2 threads by default; weaker settings are rejected at start-up).
- **Registration** turns the calling visitor into a customer in place: the user ID is kept, so the visitor's cart and history follow, and the visitor token stops validating. A caller without a visitor gets a new customer. Emails are unique per tenant.
- **Login** looks the email up in the tenant yc-gateway resolved and returns the same 401 for an unknown email and a wrong password. On the platform host (`admin.localhost` in dev) that tenant is `_platform`, so only system admins can log in there. `identity.user.login_failed.v1` is not published yet.
- **Failed sign-ins** are counted per shop and email (`login_failures`, keyed by the SHA-256 of the normalised email, under RLS). After `password.login_max_failures` (10) failures within `password.login_failure_window` (15m), logins for that email get 429 until the window has passed, even with the right password. Unknown emails are counted the same way, so the 429 doesn't reveal which emails are registered. A successful login or a completed password reset clears the count. This complements yc-gateway's per-address rate limit, which a botnet spreads across many addresses. The trade-off: anyone can lock an email out of a shop for the window. A password reset ends it. 0 turns it off.
- **The first system admin** is created by `yc-identity bootstrap-admin --email <email>`, with the password from `YC_BOOTSTRAP_ADMIN_PASSWORD` or stdin. It is idempotent: an existing email is left unchanged.
- **Shop admins** register as customers on their shop, and a system admin promotes them (`{"role": "shop_admin"}`). Only customers can be promoted, and only to shop_admin of their own tenant. Promotion publishes `identity.user.promoted.v1`, and tokens issued before it stop validating because `/internal/validate-token` checks the user's current role.
- **Listing users**: shop admins see their own tenant; system admins see every tenant or filter with `tenant_id`. Results are newest first, cursor-paginated.

### Phase 3 Implementation Notes

Added: `/refresh`, `/logout`, `/password/reset-request`, `/password/reset-confirm`, `/oauth/{provider}/start`, `/oauth/{provider}/callback` and `/internal/service-token` (migration `004_sessions_oauth_reset`). Demote and suspend landed in Phase 12; `/internal/users/{id}` is still later.

- **Refresh tokens** are opaque random strings stored by SHA-256 hash, one row per token in a *family* (one login). `/refresh` rotates: the presented token is marked replaced and a new one issued in the same family. Presenting a replaced or revoked token revokes the whole family (theft detection). Visitors get no refresh token.
- **Logout** revokes the caller's access token (its `jti`, checked by `/internal/validate-token`) and, when given, the refresh token's family. yc-gateway may accept the access token for up to its validation cache TTL.
- **Password reset** stores a single-use token by hash with `password.reset_ttl` (1h) and publishes `identity.user.password_reset_requested.v1` *without* the token (event-conventions §18); since Phase 7 yc-identity has yc-message send the link (below). `password.log_reset_links` (`YC_IDENTITY_LOG_RESET_LINKS`, refused in production) logs it. The endpoint answers 202 whether or not the email exists. Confirming sets the password, revokes every refresh token and outstanding reset token of the user, and publishes `identity.user.password_changed.v1`.
- **OAuth** (Google, Facebook) is the authorisation-code flow with PKCE. `start` stores the state (by hash), code verifier, callback URL and the calling visitor, and returns the provider URL; `redirect_url` may contain `{host}` for the shop host. `callback` consumes the state once, exchanges the code and then: logs in an existing link; else links to a user with the same email **only if the provider reports it verified** (409 otherwise, publishing `identity.oauth_linked.v1`); else turns the starting visitor into a customer in place, or creates one (`identity.user.registered.v1` with method `google`/`facebook`). Provider picture URLs are stored as `profile_image_url` (https); phone as `phone_e164` when supplied (uncommon). Phone fills only if empty; image refreshes; omitted claims do not clear stored values. `GET /me` exposes both when set. There is no customer `PATCH /me` yet.
- **Fake provider** (`oauth.<provider>.fake`, `YC_IDENTITY_OAUTH_FAKE`, development only): signs in the `login_hint` email as verified; its authorisation URL is the callback URL with `code` and `state`.
- **`identity.user.logged_in.v1`** carries `previous_visitor_id` when the login (password or OAuth) came from a visitor — the cart merge source for yc-cart.
- **Service tokens** (ADR-021): `service_clients` maps client IDs to secrets of at least 32 characters (`yc-cart: ${YC_CART_SERVICE_SECRET}`); `/internal/service-token` issues a 15-minute token with role `service` in `_platform`, and `/internal/validate-token` accepts it while the client is still configured.

### Phase 7 Implementation Notes

- **Reset emails.** After the reset request commits, yc-identity calls yc-message `POST /internal/send` (ADR-023 rule 6) as client `yc-identity` (`internal_calls`, `YC_IDENTITY_SERVICE_SECRET`) with the link built from `password.reset_url`: `{host}` is the shop host the request came in on, `{token}` the URL-escaped token (`YC_IDENTITY__PASSWORD__RESET_URL`; the dev stack uses `http://{host}/account/reset-password?token={token}`). The reference and `Idempotency-Key` are `password-reset:<request id>`. A failed send is logged and the answer is still 202. Without `internal_calls.gateway_url`, no link is sent; `password.log_reset_links` still logs it in development.
- **Service clients** now also include `yc-identity`, `yc-message`, `yc-preference` and `yc-review`.

### Phase 8 Implementation Notes

- **Deletion** (ADR-025 rules 1 and 2) is `DELETE /me` for customers and `DELETE /admin/users/{id}` for system admins (customers and shop admins; a system admin is 409), both 204. Before deleting, yc-identity asks yc-orders `GET /internal/customer-orders?user_id=` (`internal_calls`); unfinished orders or open returns are 409, no answer is 503. Deleting an already deleted user is 404.
- **What changes:** the user's email, phone, display name and password hash are cleared and the status becomes `deleted`; OAuth links, password reset tokens and sessions are removed; `pseudonym_map` records the random UUID pseudonym, who asked and when. `identity.user.deleted.v1` (with the pseudonym, role and `requested_by`) and yc-identity's own `platform.gdpr.cascade_completed.v1` (`anonymized`) are published in the same transaction. Tracking the cascade is yc-audit's (ADR-005), not yc-identity's.
- **Service clients** now also include `yc-shipping`, which asks yc-orders for a deleted customer's orders.

### Phase 12 Implementation Notes

Admin invitations and platform team (ADR-030), migration `007_invitations`.

- **Invitations** (`/sysadmin/invitations`, system_admin on the platform host): create (Idempotency-Key required), list, resend, revoke. Role pairing: `system_admin` only on `_platform`, `shop_admin` only on a shop. An email that is already an admin of that tenant is 409. Tokens are 32 random bytes stored by SHA-256 (`invitations.ttl` default 72h); only the latest token is valid. Create and resend publish `identity.user.invited.v1` and email `admin_invite` via yc-message with `invitations.accept_url` (`{host}`, `{token}`). Host is `accept_host` from the body, else `platform_hosts[0]` for `_platform`, else the shop's primary hostname projected into `shop_hosts` from `platform.tenant.provisioned.v1` / `updated.v1`.
- **Accept** (`POST /invitations/accept`, anonymous): same error for unknown/used/expired tokens; failures throttled like login. New invitees set a password; existing customers keep their `user_id` and must supply their current password. Publishes `identity.user.invitation_accepted.v1` (includes email). Consuming `platform.tenant.provisioned.v1` with `primary_admin_email` creates the first shop-admin invite as the envelope actor.
- **Suspend / reactivate / demote** (`POST /admin/users/{id}/suspend|reactivate|demote`, system_admin): cannot suspend yourself or the last active system admin; suspend revokes refresh families; demote is `shop_admin` → `customer` only. Publishes `suspended.v1`, `reactivated.v1`, `demoted.v1`.
- **Gateway registration:** `/sysadmin/invitations` via KeepRegistered; accept stays on the gateway static chain.

## Database Schema

Database: **`yc_identity`**

```sql
CREATE TABLE users (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       TEXT NOT NULL,
    email           TEXT,                    -- NULL for pure visitors
    phone_e164      TEXT,
    password_hash   TEXT,                    -- NULL for OAuth-only or visitor
    role            TEXT NOT NULL,           -- 'visitor','customer','shop_admin','system_admin'
    status          TEXT NOT NULL DEFAULT 'active',  -- 'active','suspended','deleted'
    display_name    TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at      TIMESTAMPTZ,             -- soft delete marker for GDPR audit
    UNIQUE (tenant_id, email)
);
CREATE INDEX idx_users_tenant ON users (tenant_id);
CREATE INDEX idx_users_email ON users (tenant_id, email);

CREATE TABLE oauth_identities (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider        TEXT NOT NULL,           -- 'google','facebook'
    provider_sub    TEXT NOT NULL,           -- subject ID from provider
    email_at_link   TEXT,
    linked_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (provider, provider_sub)
);

CREATE TABLE refresh_tokens (
    jti             UUID PRIMARY KEY,
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    tenant_id       TEXT NOT NULL,
    issued_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at      TIMESTAMPTZ NOT NULL,
    revoked_at      TIMESTAMPTZ,
    replaced_by     UUID                     -- for rotation
);
CREATE INDEX idx_refresh_tokens_user ON refresh_tokens (user_id);

CREATE TABLE password_reset_tokens (
    token_hash      TEXT PRIMARY KEY,
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at      TIMESTAMPTZ NOT NULL,
    used_at         TIMESTAMPTZ
);

CREATE TABLE pseudonym_map (
    -- GDPR: map deleted user IDs to pseudonymous tokens for order/payment retention
    user_id         UUID PRIMARY KEY,
    pseudonym       TEXT NOT NULL UNIQUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Standard tables from yc-eventbus
CREATE TABLE outbox ( ... );            -- see yc-eventbus spec
CREATE TABLE processed_events ( ... );   -- see yc-eventbus spec

-- RLS: enabled on users, oauth_identities, refresh_tokens for tenant isolation
ALTER TABLE users ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON users
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform');
-- similar policies on other tenant-scoped tables
```

Migrations managed via `golang-migrate`, in `/services/yc-identity/migrations/`.

## Password Storage

- Argon2id (recommended parameters, revised yearly)
- No plaintext passwords in logs or events

## Published Events

| Event | When |
|---|---|
| `identity.visitor.created.v1` | Visitor token issued |
| `identity.user.registered.v1` | New customer registration |
| `identity.user.promoted.v1` | Role changed upward |
| `identity.user.demoted.v1` | Shop admin demoted to customer (ADR-030) |
| `identity.user.invited.v1` | Invitation created or resent (ADR-030) |
| `identity.user.invitation_accepted.v1` | Invitation accepted (ADR-030) |
| `identity.user.invitation_revoked.v1` | Pending invitation revoked (ADR-030) |
| `identity.user.logged_in.v1` | Successful login |
| `identity.user.login_failed.v1` | Failed login (for brute-force detection) |
| `identity.user.password_reset_requested.v1` | Reset flow started |
| `identity.user.password_changed.v1` | Password successfully changed |
| `identity.user.suspended.v1` | Admin-suspended (ADR-030) |
| `identity.user.reactivated.v1` | Suspended user reactivated (ADR-030) |
| `identity.user.deleted.v1` | Deletion triggered — kicks off GDPR cascade |
| `identity.oauth_linked.v1` | OAuth provider linked to user |
| `identity.oauth_unlinked.v1` | OAuth provider unlinked |

## Subscribed Events

| Event | Action |
|---|---|
| `platform.gdpr.cascade_completed.v1` | Track cascade completion per deleted user |
| `platform.tenant.provisioned.v1` | When `primary_admin_email` is set, create the first shop-admin invitation (ADR-030) |
| `platform.tenant.deleted.v1` | Mark all tenant users as deleted; initiate per-user cascade |

## Configuration

Environment variables / config file:

```yaml
yc-identity:
  database:
    url: postgres://yc_identity:...@...
  jwt:
    private_key_path: /etc/yc/jwt-private.pem
    public_key_path: /etc/yc/jwt-public.pem
    access_token_ttl_minutes: 15
    refresh_token_ttl_days: 30
  oauth:
    google:
      client_id: ...
      client_secret: ...
      redirect_url: ...
    facebook:
      client_id: ...
      client_secret: ...
      redirect_url: ...
  password:
    argon2:
      time: 2
      memory_kb: 65536
      threads: 2
  nats:
    url: nats://...
  log_level: info
```

Tenant-overridable settings stored in `yc_identity` database and queried per request:
- Access token TTL
- Refresh token TTL
- Password policy
- OAuth providers enabled

## Dependencies on Other Services

- **None at startup** — this is a foundational service
- **Reads from**: nothing (self-contained)
- **Calls**: none directly (OAuth providers via HTTPS)

## Called By

- `yc-gateway` (via `/internal/validate-token`)
- All services indirectly (via gateway token validation)

## GDPR Behavior

On `DELETE /user/me` or admin deletion:
1. Verify requester
2. Generate pseudonym for the user (store in `pseudonym_map`)
3. Mark user `status = 'deleted'`, clear PII fields (email, phone, display_name, password_hash)
4. Revoke all refresh tokens
5. Publish `identity.user.deleted.v1` with both `user_id` and `pseudonym`
6. Track incoming `platform.gdpr.cascade_completed.v1` events from downstream services
7. Alert if cascade incomplete after 72 hours

## Open Questions / Future Scope

- Multi-factor authentication (TOTP, SMS)
- Social auth beyond Google/Facebook (Apple, Microsoft)
- Shop admin scoping to multiple tenants
- Session revocation across devices
- Account merging when a visitor registers after guest orders
