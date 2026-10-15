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

At startup, each backend service calls the gateway's **internal listener** (`internal_http.listen`, default `:8091`; ADR-020 rule 6) through `httpkit.KeepRegistered`:

```
POST /internal/register
{
  "service_name": "yc-product",
  "instance_id": "yc-product-7d9f",          // defaults to the hostname
  "base_url": "http://yc-product:8080",
  "version": "1.4.0",
  "health_url": "/healthz",                  // path under base_url
  "routes": [
    { "prefix": "/products", "methods": ["GET"] },
    { "prefix": "/admin/products", "roles": ["shop_admin", "system_admin"] },
    { "prefix": "/login", "methods": ["POST"], "anonymous": true }
  ]
}
→ 204; 400 for an invalid body or a static prefix; 409 for another service's prefix
```

Route fields mean what they mean in static config: longest prefix wins, empty `methods` admits any method, empty `roles` admits any actor (visitors included), `anonymous` admits token-less requests on platform hosts.

- Registrations are held in memory, one per service name; a registration replaces the service's previous routes. Services re-register every `gateway.interval` (default 30 s), so a restarted gateway relearns them; an unchanged re-registration is a no-op.
- Routes are **also** declared in a static configuration file for cold-start availability — self-registration **augments** static config, and a registered route may not reuse a static prefix.
- The gateway polls each registration's `health_url` every `registry.health_interval` (5 s) and deregisters after `registry.failure_threshold` (3) consecutive failures. Services call `POST /internal/deregister {"service_name", "instance_id"}` on shutdown.
- Registration and deregistration publish `platform.service.registered.v1` / `platform.service.deregistered.v1` through the outbox.
- Each gateway instance holds only the registrations it received (ADR-020 rule 7).

### Platform Hosts

Hostnames in `platform_hosts` resolve to `_platform` (ADR-020 rule 5). They never issue visitor tokens: a request without a bearer token reaches only `anonymous` routes (e.g. `POST /login`) and is forwarded with `X-YC-Tenant-ID: _platform` and no actor; any other route returns 401. Bearer tokens must be `_platform` tokens, which only system admins hold. System admins act on a shop by naming its tenant in the request, never by using a shop host.

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

ADR-021. When service A needs to call service B:
- Service A gets a short-lived service token from `POST /internal/service-token` on the gateway's internal listener (proxied to yc-identity) with its client ID and secret
- It calls B's `/internal/` route on the internal listener with that token, plus the tenant, actor and correlation headers of the request it is serving (only the tenant for its own work; the actor then becomes the service)
- The gateway validates the token (actor type `service`), requires `X-YC-Tenant-ID`, removes `Authorization`, sets `X-YC-Caller-Service` and forwards to the service that registered the route
- `httpkit.InternalClient` does all of this; the caller's own access token is never forwarded
- Direct service-to-service calls are rejected for v1 to keep auth and observability consistent

## API

### Public Endpoints

Dynamically defined by registered services. Gateway has no fixed public API beyond:

| Method | Path | Description |
|---|---|---|
| GET | `/healthz` | Gateway health |
| GET | `/readyz` | Gateway readiness |
| GET | `/version` | Build info |

### Internal Endpoints

Served only on the internal listener (`:8091`); the public listener never routes `/internal/`.

| Method | Path | Description | Auth |
|---|---|---|---|
| POST | `/internal/register` | Register a service and its routes | network placement (service token in Phase 4) |
| POST | `/internal/deregister` | Deregister a service instance | network placement (service token in Phase 4) |
| GET | `/internal/routes` | List static and registered routes | network placement |
| POST | `/internal/service-token` | Proxied to yc-identity: client credentials → service token (ADR-021) | client secret |
| any | `/internal/*` (other) | Proxied to the service that registered the prefix (ADR-021) | service token |

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
- Service registrations are held in memory. On restart, services re-register within `gateway.interval` (static routes apply meanwhile).
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

Implemented in Phase 3 on the public listener (`service.RateLimiter`, `golang.org/x/time/rate`):

- A request must fit its actor's bucket and then its tenant's; a denial cancels both reservations, so a throttled actor does not use up the tenant's quota.
- Requests without credentials are limited by client address *before* a visitor is issued, so floods cannot mint visitors. Platform-host (`_platform`) requests are limited by address. Everything else is limited by actor ID after the token is validated; visitors use the `visitor` quota.
- Over quota: `429` with `RATE_LIMITED` and `Retry-After` in whole seconds.
- Buckets idle for `idle_ttl` are swept. The internal listener is not limited.
- Defaults: tenant 200/s burst 400, actor 600/min burst 60, visitor 120/min burst 30; `tenant_overrides` per tenant. `YC_GATEWAY_RATE_LIMIT=false` turns limiting off.

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
internal_http:                  # registration endpoints; never expose publicly
  listen: ":8091"
database:
  url: ${YC_GATEWAY_DB_URL}
upstreams:                      # backend base URLs by name
  identity: http://yc-identity:8080
routes:                         # longest prefix wins; empty roles admit any actor
  - prefix: /me
    methods: [GET]
    upstream: identity
  - prefix: /login
    methods: [POST]
    upstream: identity
    anonymous: true             # reachable without a token on platform hosts
registry:                       # health polling of registered services
  health_interval: 5s
  health_timeout: 2s
  failure_threshold: 3
platform_hosts:                 # resolve to _platform; system admins only
  - admin.example.com
tenant_hosts:                   # static hostname → tenant, before the tenant_hosts table
  - hostname: shop1.example.com
    tenant_id: shop1
tenant_hosts_refresh: 30s
token_cache_ttl: 60s
identity_timeout: 3s
visitor_cookie:
  name: yc_visitor
  secure: ${YC_GATEWAY_COOKIE_SECURE:-true}   # false only for plain-HTTP local dev
rate_limit:
  enabled: ${YC_GATEWAY_RATE_LIMIT:-true}
  tenant:  { requests: 200, per: 1s, burst: 400 }
  actor:   { requests: 600, per: 1m, burst: 60 }
  visitor: { requests: 120, per: 1m, burst: 30 }
  tenant_overrides: {}          # tenant ID → quota
  idle_ttl: 10m
nats:
  url: ${NATS_URL}
```

Tenant resolution is by hostname only.

### Phase 3 Implementation Notes

- **Internal routing (ADR-021).** Registered prefixes under `/internal/` go to a separate table that only the internal listener consults; they must name a service, carry no roles and not overlap the gateway's own internal paths. The public listener rejects service tokens with 401.
- **Login routing.** Static identity routes now include `/refresh`, `/password` (both anonymous) and `/logout`, `/oauth` alongside `/login`, `/register` and `/visitor`. OAuth starts and callbacks carry the visitor cookie, so yc-identity can name the visitor as the cart merge source.
- **Rate limiting** as described under Rate Limiting.

### Phase 8 Implementation Notes

- **Unserved internal paths** (ADR-025 rule 18): on the internal listener, a path no registered service serves is `503 unavailable` ("No service serves this path"), not 404, so a caller retries while the owning service restarts or re-registers. The public listener keeps 404.
- **Monitoring:** yc-monitor learns every registered service from `platform.service.registered.v1` and `.deregistered.v1` and checks it independently of the gateway's own health polling.

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
