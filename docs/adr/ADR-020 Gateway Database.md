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
