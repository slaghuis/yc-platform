# ADR-029: Tenant Registry

**Status:** Proposed
**Date:** 2026-10-15
**Context:** In v1 a shop exists only as the side effects of one event. `yc-tenant-cli provision` publishes `platform.tenant.provisioned.v1` straight to NATS (no outbox, no record), and each service keeps its own fragment: yc-gateway the hostnames (`tenant_hosts`, ADR-020), yc-pricing the pricing config, yc-analytics `dim_tenant`, and so on. Nothing can list shops, show one, rename one, change its hostnames, suspend it or delete it. The service docs list subscriptions to `platform.tenant.suspended.v1` and `platform.tenant.deleted.v1`, but neither schema exists and no service implements them. The system admin console (yc-platform-admin) needs shop management as an API, and yc-message needs each shop's real hostname for links (ADR-027 rule 14).

## Decision

**A new service, `yc-tenant`, owns the canonical record of every shop and its lifecycle. It exposes `/sysadmin/tenants` and publishes tenant lifecycle events through its outbox. Every other service keeps its own projection, fed by those events. `yc-tenant-cli` becomes a client of the API.**

## Rules

### Record

1. **One row per tenant** in `yc_tenant.tenants` (ADR-017): `tenant_id` (the ADR-001 slug, immutable), `name`, `status`, `store_currency`, `default_jurisdiction`, `is_tax_inclusive`, `contact_email`, `created_at`, `updated_at`, and the hostnames in `tenant_hostnames` with exactly one `is_primary`. The table is platform-wide, without RLS, like `tenant_hosts` (ADR-020 rule 4).
2. **Immutable after creation:** `tenant_id` and `store_currency` (yc-pricing fixes the currency once prices exist; ADR-014). Jurisdiction and tax-inclusive can change only through the owning services' admin APIs, not here.
3. **Hostnames** are lowercase, unique across all tenants and never a platform host. yc-tenant checks uniqueness before publishing, so a duplicate is a `409` instead of a dead-lettered event (the v1 failure mode).

### Lifecycle

4. **States:** `active` → `suspended` → `active` (reinstated), and `suspended` → `deleted`. A tenant must be suspended before it can be deleted; `deleted` is final and the `tenant_id` is never reused.
5. **Suspended** means yc-gateway answers every request for the tenant's hosts with `503 tenant_suspended` (api-conventions §5) and refuses webhooks the same way. Data is untouched, and sagas already running are left to finish or expire.
6. **Deleted** means yc-gateway refuses the hosts permanently (`410`) and frees the hostnames. In v1.1 data stays in each service until the deletion cascade is built (backlog-v2 P2, "Tenant lifecycle subscriptions"); yc-tenant records `data_purged_at` as null until then.

### API

7. **System admins only**, on platform hosts:

   | Method | Path | Effect |
   |---|---|---|
   | GET | `/sysadmin/tenants` | List, filter by `status`, search by name or hostname; cursor-paginated |
   | POST | `/sysadmin/tenants` | Create; body as `platform.tenant.provisioned.v1` plus `contact_email`; `Idempotency-Key` required |
   | GET | `/sysadmin/tenants/{id}` | Read |
   | PATCH | `/sysadmin/tenants/{id}` | Change `name`, `contact_email` |
   | PUT | `/sysadmin/tenants/{id}/hostnames` | Replace hostnames and the primary |
   | POST | `/sysadmin/tenants/{id}/suspend` | `reason` required |
   | POST | `/sysadmin/tenants/{id}/reinstate` | |
   | DELETE | `/sysadmin/tenants/{id}` | Only when suspended; `confirm_tenant_id` must repeat the ID |

8. **Every change is audited**: the lifecycle events below carry the acting system admin in the envelope, and yc-audit records them as `tenant_lifecycle` (today it records only `provisioned`; the new types join its subscriptions).

### Events

9. **Published through the outbox** on `_platform` (ADR-006, ADR-008):
   - `platform.tenant.provisioned.v1`: unchanged schema; now published by yc-tenant. It adds no fields, so existing consumers are untouched. `hostnames[0]` is the primary.
   - `platform.tenant.updated.v1` (new): name, hostnames, primary hostname, after the change.
   - `platform.tenant.suspended.v1`, `platform.tenant.reinstated.v1`, `platform.tenant.deleted.v1` (new): tenant, reason (suspend), actor.
10. **Consumers in v1.1:**
    - yc-gateway: hostnames from `provisioned` and `updated` (replacing the tenant's rows in `tenant_hosts`), 503 on `suspended`, cleared on `reinstated`, 410 on `deleted`. The suspended set is kept in memory and in `yc_gateway`, so a restart keeps it.
    - yc-message: a projection of each shop's name and primary hostname, used for `shop_url` and every link (ADR-027 rule 14). `messages.shop_url` remains only as the fallback for a tenant the projection hasn't seen yet.
    - yc-identity: `provisioned` with `primary_admin_email` sends the first shop admin an invitation (ADR-030).
    - yc-analytics: name changes in `dim_tenant`.
    Other services keep their v1 `provisioned` handling; their `suspended`/`deleted` handling stays in backlog-v2.

### Migration

11. **Existing tenants** are imported once by `yc-tenant import`, which reads yc-gateway's static and database `tenant_hosts` and yc-pricing's configs and writes records without publishing `provisioned` again.
12. **`yc-tenant-cli`** keeps its flags but calls `POST /sysadmin/tenants` with a system admin token (`YC_TENANT_CLI_TOKEN`). It no longer publishes to NATS.

## Consequences

**Positive**
- The console gets a real shop list and lifecycle; suspension finally does something.
- Provisioning goes through an outbox, so a NATS outage no longer loses a new shop.
- Hostname conflicts become a synchronous `409` instead of a dead-lettered event.
- yc-message can link to shops on their own domains.

**Negative**
- An 18th service and database, with its own migrations, image and runbook entries.
- Deletion in v1.1 refuses traffic but does not purge data, so "deleted" must be explained as such in the console and runbooks until the cascade exists.
- yc-gateway's tenant handling grows (updated, suspended, reinstated, deleted), on a component every request passes through. Rule 1 of ADR-020 still applies: all of it is served from memory.

## Alternatives Considered

- **Keep the registry in yc-gateway** (it already stores `tenant_hosts`): rejected; it would put admin CRUD and lifecycle logic into the one component every request depends on, against ADR-020 rule 1's intent.
- **Keep it in yc-identity**: rejected; shops are not identities, and yc-identity is already the most security-sensitive service.
- **No registry; the console reads `dim_tenant` from yc-analytics or `tenant_hosts` from the gateway**: rejected; neither owns the data, neither has lifecycle, and both are projections that can lag.
- **Hard delete with a full cascade now**: rejected for v1.1; it needs handlers in eleven services and retention rules per domain (ADR-004). It stays in backlog-v2.
