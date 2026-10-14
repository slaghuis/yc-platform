# yc-gateway — Service Requirement

## Overview

`yc-gateway` is the single entry point for all client traffic. It authenticates callers (via yc-identity), issues access tokens, maintains service registration, routes API calls to backend services, and propagates auth context through the call chain.

## Location in Monorepo

```
/services/yc-gateway/
```

## Responsibilities

1. Accept incoming client requests
2. Extract or issue access tokens
3. Validate access tokens via yc-identity
4. Route requests to the appropriate backend service based on registered routes
5. Propagate the access token to the backend service
6. Allow backend services to register their routes dynamically at startup
7. Allow service-to-service calls with token propagation
8. Apply rate limiting per tenant and per actor
9. Emit per-request metrics and traces
10. Return a visitor token to any unauthenticated caller

## Multi-Tenancy

- Each incoming request must resolve to a tenant
- Tenant resolution strategies (configurable; one is active per deployment):
  - **Hostname-based**: `tenant1.shops.com` → tenant_id `tenant1`
  - **Path-prefix-based**: `/t/tenant1/...` → tenant_id `tenant1`
  - **Header-based**: `X-YC-Tenant: tenant1`
- **Recommendation for v1**: hostname-based with a mapping table managed by system admins
- Platform/admin APIs resolve to `_platform` tenant (restricted to system_admin role)

## Authentication Flow

```
Client → Gateway: request
  Gateway: strip inbound X-YC-Tenant-ID / X-YC-Actor-* headers
  Gateway: resolve tenant from Host (unknown host → 404)
  Gateway: match route (no route, or any /internal/ path → 404)

  if Authorization: Bearer <token>:
      Gateway → yc-identity: /internal/validate-token
      invalid or expired → 401 (client should refresh or re-login)
      token.tenant_id != resolved tenant → 403
  else if visitor cookie (yc_visitor) present and valid for this tenant:
      proceed with that visitor
  else (no token, or stale / other-tenant cookie):
      Gateway → yc-identity: POST /visitor for the resolved tenant
      Gateway → Client: Set-Cookie yc_visitor (HttpOnly, SameSite=Lax, Secure)

  Gateway: enforce route roles (visitor → 401, other actor → 403)
  Gateway → backend service: forward with Authorization and X-YC-* headers;
           the visitor cookie is removed from the forwarded Cookie header
  backend service → Gateway → Client: response
```

The visitor cookie is the gateway's own state, so a stale one is replaced rather than rejected; an explicit bearer token is never silently replaced.

Token validation results are **cached in memory** (default TTL: 60 seconds, never beyond the token's expiry) keyed by a SHA-256 of the token. Rejections are not cached. Revocation becomes effective within the cache TTL. Trade-off is acceptable; alternative is a Redis-backed revocation list, deferred to future scope.

## Routing

### Service Registration

At startup, each backend service calls:

```
POST /internal/register
{
  "service_name": "yc-product",
  "base_url": "http://yc-product:8080",
  "routes": [
    { "method": "GET",  "path": "/products",       "auth": "any" },
    { "method": "POST", "path": "/admin/products", "auth": "shop_admin" },
    ...
  ],
  "health_url": "/healthz"
}
```

- Routes are stored in-memory in the gateway
- Routes are **also** declared in a static configuration file for cold-start availability — service self-registration **augments** but does not replace static config
- Backend service deregistration happens automatically on health check failure (3 consecutive failures over 15 seconds)

### Request Routing

- Longest-prefix path match
- Method must match
- Auth level is enforced before forwarding
- Gateway adds standard headers to the forwarded request:
  - `Authorization: Bearer <access_token>`
  - `X-YC-Tenant-ID: <tenant>`
  - `X-YC-Correlation-ID: <uuid>`
  - `X-YC-Actor-ID: <user_id>`
  - `X-YC-Actor-Type: <visitor|customer|shop_admin|system_admin>`

### Service-to-Service Calls

When service A needs to call service B:
- Service A calls back through the gateway using the same `Authorization` header it received
- Gateway re-validates and forwards to service B
- This preserves the auth chain and keeps gateway as the single routing authority
- Alternative (direct service-to-service) is rejected for v1 to maintain consistent auth and observability

## API

### Public Endpoints

Dynamically defined by registered services. Gateway has no fixed public API beyond:

| Method | Path | Description |
|---|---|---|
| GET | `/healthz` | Gateway health |
| GET | `/readyz` | Gateway readiness |
| GET | `/version` | Build info |

### Internal Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| POST | `/internal/register` | Register a service and its routes | service token |
| POST | `/internal/deregister` | Deregister a service | service token |
| GET | `/internal/routes` | List all registered routes | system_admin |

## Database

Database: **`yc_gateway`** (ADR-020). Small and platform-wide; the request path never reads it.

```sql
CREATE TABLE tenant_hosts (
    hostname    TEXT PRIMARY KEY,        -- lowercase, no port
    tenant_id   TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_tenant_hosts_tenant ON tenant_hosts (tenant_id);

-- Standard tables from yc-eventbus
CREATE TABLE outbox ( ... );
CREATE TABLE processed_events ( ... );
```

- `tenant_hosts` is written by the `platform.tenant.provisioned.v1` handler and loaded into memory at startup, after `tenant_hosts` from config, and reloaded every `tenant_hosts_refresh` so every instance learns new tenants. It has no RLS: it is read before a tenant is known.
- A provisioning event that claims a hostname owned by another tenant (in config or `tenant_hosts`) is rejected whole and dead-lettered as poison.
- Service registrations are held in memory. On restart, services re-register (or static config is loaded).
- Token validation results are cached in memory only.

## Published Events

| Event | When |
|---|---|
| `platform.service.registered.v1` | A backend service registers |
| `platform.service.deregistered.v1` | A backend service deregisters or fails health |
| `monitor.gateway.request_received.v1` | **Not published per request** — sampled metrics only, aggregated by yc-monitor |

## Subscribed Events

| Event | Action |
|---|---|
| `platform.tenant.provisioned.v1` | Register tenant hostname mapping if applicable |
| `platform.tenant.suspended.v1` | Block all traffic for that tenant (return 503) |
| `platform.tenant.deleted.v1` | Permanently refuse traffic for that tenant |

## Rate Limiting

- Per-tenant quota (requests/second), configurable per tenant
- Per-actor quota (requests/minute per user), configurable globally
- Stricter default limits for visitor tokens (DoS protection)
- Enforcement via in-memory token bucket (per gateway instance); for multi-instance gateways, replace with Redis-backed limiter — deferred to future scope but noted

## Observability

- OpenTelemetry middleware wraps every request
- Each request generates a trace with the correlation ID
- Correlation ID is propagated to backend services via `X-YC-Correlation-ID`
- Metrics: request count, request latency, error rate — per route, per tenant
- Access log in structured JSON

## Configuration

Defaults live in `services/yc-gateway/config/config.default.yaml`; override with `YC_GATEWAY__<SECTION>__<KEY>` (for example `YC_GATEWAY__UPSTREAMS__IDENTITY`).

```yaml
http:
  listen: ":8080"
database:
  url: ${YC_GATEWAY_DB_URL}
upstreams:                      # backend base URLs by name
  identity: http://yc-identity:8080
routes:                         # longest prefix wins; empty roles admit any actor
  - prefix: /me
    methods: [GET]
    upstream: identity
  # - prefix: /admin/products
  #   upstream: product
  #   roles: [shop_admin]
tenant_hosts:                   # static hostname → tenant, before the tenant_hosts table
  - hostname: shop1.example.com
    tenant_id: shop1
tenant_hosts_refresh: 30s
token_cache_ttl: 60s
identity_timeout: 3s
visitor_cookie:
  name: yc_visitor
  secure: ${YC_GATEWAY_COOKIE_SECURE:-true}   # false only for plain-HTTP local dev
nats:
  url: ${NATS_URL}
```

Tenant resolution is by hostname only. Rate limits are not yet implemented (see Rate Limiting).

## Dependencies on Other Services

- **yc-identity** (critical) — for token validation and visitor token issuance

## Called By

- All external clients
- All services (for service-to-service calls)

## GDPR Behavior

- Gateway holds no PII at rest
- Access logs may include user IDs — logs are rotated and anonymized after 30 days
- No cascade subscription needed

## Open Questions / Future Scope

- Redis-backed token cache and rate limiter (for multi-instance scale-out)
- WebSocket support for real-time features
- GraphQL federation layer (if we adopt GraphQL)
- CORS policy configuration per tenant
- API versioning strategy (currently assumed path-based `/v1/...`)
- Admin UI route separation
