# ADR-020: Gateway Database

**Status:** Accepted
**Date:** 2026-10-14
**Context:** ADR-017 lists yc-gateway as stateless ("— (stateless)"), and `yc-gateway.md` says "Database: None". Two gateway responsibilities need durable state:

1. **Publishing.** yc-gateway publishes `platform.service.registered.v1` and `platform.service.deregistered.v1`. Without a database it has no outbox (ADR-006), so these events would be published directly to NATS and lost whenever NATS is unavailable at the moment of registration.
2. **Tenant resolution.** The spec recommends hostname-based tenant resolution "with a mapping table managed by system admins", fed by `platform.tenant.provisioned.v1`. Without a database, the mapping only exists in static config and in memory, and is lost on restart. Consuming the provisioning event idempotently also needs the `processed_events` table.

## Decision

**yc-gateway owns a small Postgres database, `yc_gateway`, under the same rules as every other service (ADR-017).** It holds:

| Table | Purpose | Tenant-scoped |
|---|---|---|
| `tenant_hosts` | Hostname → tenant mapping, written by the `platform.tenant.provisioned.v1` handler | No — platform-wide; it is read before a tenant is known |
| `outbox` | Transactional outbox for gateway-published events (ADR-006) | Rows carry `tenant_id` |
| `processed_events` | Consumer idempotency (yc-eventbus) | — |

## Rules

1. **Request path stays database-free.** The gateway serves tenant resolution, routing and token-validation caching from memory. The database is read at startup and written by event handlers; a slow or unavailable database must not add latency to proxied requests.
2. **Static config still wins at cold start.** `tenant_hostname_map` in config is loaded first; database rows add to it. A hostname present in both must map to the same tenant, or startup fails.
3. **No session or token storage.** Token validation results stay in the in-memory cache described in `yc-gateway.md`; revocation lists remain future scope.
4. **`tenant_hosts` has no RLS.** It is a platform-wide table (ADR-017 allows this for "truly platform-wide tables"); only the gateway's provisioning handler writes it.
5. **Platform hosts.** Hostnames listed in `platform_hosts` resolve to `_platform` (`yc-gateway.md`, Multi-Tenancy). Only system admins hold `_platform` tokens, so the existing rule "token tenant must equal host tenant" confines platform hosts to system admins. A platform host never issues visitor tokens: a request without a bearer token proceeds with no actor and reaches only routes marked `anonymous` (e.g. `/login`). System admins act on a shop by naming its tenant explicitly (`POST /sysadmin/suppliers {"tenant_id": ...}`), never by presenting a `_platform` token on a shop host.
6. **Internal listener.** Registration endpoints (`/internal/register`, `/internal/deregister`, `/internal/routes`) are served on a second listener (`internal_http.listen`, default `:8091`) that must not be exposed outside the platform network. Until service tokens exist (ADR-019's internal-call client, Phase 4), network placement is their only protection. The public listener still never routes `/internal/`. *(ADR-021 adds service tokens and routes other `/internal/` calls on this listener; registration stays on network placement.)*
7. **Registrations live in memory.** A registration replaces the service's previous routes and is published as `platform.service.registered.v1` through the outbox. The gateway polls each registered `health_url` every 5 seconds and deregisters after 3 consecutive failures (`platform.service.deregistered.v1`). Static routes are never deregistered, and a registered route may not reuse a static route's prefix. With several gateway instances each instance holds only the registrations it received; services register with every instance, or rely on static routes, until registrations are shared.

*Amended 2026-10-14 (Phase 2): rules 5–7.*

## Consequences

**Positive**
- Gateway events go through the outbox like every other service's; no direct-publish exception.
- Tenant hostname mappings survive restarts and do not require a config redeploy per tenant.
- The gateway follows the standard service pattern (migrations, `/readyz` database check), so there is one less special case for the AI code factory.

**Negative**
- The gateway's readiness now depends on Postgres. Mitigated by rule 1: once started, proxying continues from memory if the database becomes unavailable; only new provisioning events wait.
- One more database to back up (small, platform-wide).

## Amends

- ADR-017's service-to-database map: YC-Gateway → `yc_gateway`.
- `docs/services/yc-gateway.md` "Database" section.
