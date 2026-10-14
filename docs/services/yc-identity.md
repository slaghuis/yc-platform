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
| GET | `/internal/users/{id}` | Fetch minimal user info | service |

OpenAPI spec stub lives at `/services/yc-identity/api/openapi.yaml`.

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
| `identity.user.demoted.v1` | Role changed downward |
| `identity.user.logged_in.v1` | Successful login |
| `identity.user.login_failed.v1` | Failed login (for brute-force detection) |
| `identity.user.password_reset_requested.v1` | Reset flow started |
| `identity.user.password_changed.v1` | Password successfully changed |
| `identity.user.suspended.v1` | Admin-suspended |
| `identity.user.deleted.v1` | Deletion triggered — kicks off GDPR cascade |
| `identity.oauth_linked.v1` | OAuth provider linked to user |
| `identity.oauth_unlinked.v1` | OAuth provider unlinked |

## Subscribed Events

| Event | Action |
|---|---|
| `platform.gdpr.cascade_completed.v1` | Track cascade completion per deleted user |
| `platform.tenant.provisioned.v1` | Prepare tenant-scoped state (if needed) |
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
- Rate limiting on login attempts (currently expected to live in yc-gateway)
- Account merging when a visitor registers after guest orders
