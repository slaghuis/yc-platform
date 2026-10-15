# ADR-021: Service Tokens and Internal Calls

**Status:** Accepted
**Date:** 2026-10-15
**Context:** `api-conventions.md` §3 reserves `/internal/` for service-to-service calls authenticated with a "service token", and ADR-019 rule 1 has yc-checkout call participants "through yc-gateway with its service token". Neither document says what a service token is, who issues it, or how the gateway routes `/internal/` (the public listener never routes it, ADR-020 rule 6). The implementation plan built this with yc-checkout in Phase 4, but Phase 3's yc-cart already needs a synchronous call: it snapshots prices from yc-pricing when an item is added and compares them on every read. Calling yc-pricing's public endpoint directly would bypass the gateway and set a precedent every later service would copy.

## Decision

**yc-identity issues short-lived service tokens to configured service clients, and yc-gateway's internal listener routes `/internal/` calls that carry one.** Services make these calls through a shared client in `yc-httpkit`.

## Rules

1. **Service clients.** yc-identity's config lists service clients by name with a secret each (`service_clients: {yc-cart: ${YC_CART_SERVICE_SECRET}}`). A secret must be at least 32 characters. Removing a client from the config revokes its tokens at the next validation.

2. **Service tokens.** `POST /internal/service-token {client_id, client_secret}` returns an RS256 JWT signed with the access-token key: `sub` is the service name, `tenant_id` is `_platform`, `role` and `actor_type` are `service`, TTL 15 minutes. Wrong credentials get the same 401 whatever the cause. `/internal/validate-token` accepts service tokens, checking that the client is still configured instead of looking up a user.

3. **Routing.** yc-gateway's internal listener (`:8091`, ADR-020 rule 6) serves:
   - `POST /internal/service-token`, proxied to yc-identity without authentication;
   - its own registration endpoints, still protected only by network placement;
   - every other `/internal/` path, routed to the service that registered it. These require a valid service token; anything else is 401.

   Services register internal routes alongside their public ones, with prefixes under `/internal/`. They are kept in a separate table that only the internal listener consults, so the public listener still never routes `/internal/`.

4. **Headers.** The caller sends the tenant, actor and correlation headers of the request it is serving (`api-conventions.md` §9). For work of its own, such as an event handler, it sends only the tenant, and the actor becomes the service itself. `X-YC-Tenant-ID` is required on every routed internal call. The gateway passes these headers through, removes the `Authorization` header and sets `X-YC-Caller-Service` to the token's service name. Backends trust them for the same reason they trust the public listener's headers: only the gateway can reach them.

5. **Audience.** `authctx.Audience` admits `/internal/` requests only when `X-YC-Caller-Service` is present. yc-gateway sets it on its own calls to yc-identity (`yc-gateway`) and on proxied service-token requests. The public listener strips it, along with the other identity headers.

6. **Client.** `httpkit.InternalClient` fetches a token with its client credentials, caches it until shortly before expiry, and fetches a new one once if a call returns 401. It propagates headers from the context as rule 4 describes and applies the standard client timeout. `Call` takes per-call options: an `Idempotency-Key` sent unchanged on every attempt (ADR-019 rule 5), a per-attempt timeout, and a number of attempts. It retries network errors, timeouts and 5xx responses with doubling backoff (200 ms by default), never a 4xx, and stops when the caller's context ends. *(Amended in Phase 4, when yc-checkout and yc-orders started calling.)*

7. **No per-route caller lists yet.** Any service token may call any internal route. Restricting routes to named callers is future work, should a service ever hold data another service must not read.

## Consequences

**Positive**
- One authenticated path for service-to-service calls, built before yc-checkout depends on it.
- Callers need one URL (the gateway's internal listener) and one credential, whatever they call.
- The gateway's correlation, tracing and future rate limiting cover internal traffic too.

**Negative**
- Internal calls take an extra hop through the gateway.
- Shared secrets in configuration must be rotated by redeploying both yc-identity and the caller.
- Registration endpoints stay protected only by network placement until every service holds a client secret.

## Alternatives Considered

- **Direct calls to the target's public endpoint with forwarded headers:** rejected. It bypasses the gateway and relies entirely on network placement, which is the precedent ADR-019 rule 1 rules out.
- **Mutual TLS between services:** rejected for now. It would need certificate issuance and rotation across the platform, and still would not carry the end user's identity.
- **Local projections instead of calls:** used where it fits (yc-cart keeps display currency from preference events), but pricing logic cannot be copied into every caller.
