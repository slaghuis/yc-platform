# yc-tenant — Service Requirement

## Overview

`yc-tenant` owns the canonical record of every shop and its lifecycle (ADR-029). It exposes `/sysadmin/tenants` for system admins and publishes tenant lifecycle events through its outbox on `_platform`. Every other service keeps its own projection, fed by those events. Shop admins manage optional seller invoice identity (`vat_number`, `invoice_address`) on `/admin/shop` (issue #29); yc-orders snapshots that via `GET /internal/tenants/{id}` at order create.

## Location in Monorepo

```
/services/yc-tenant/
```

## Responsibilities

1. Keep one record per shop: `tenant_id`, name, status, currency, jurisdiction, tax-inclusive flag, contact email and hostnames
2. Enforce hostname rules: lowercase, unique across all tenants, never a platform host (`409` otherwise)
3. Run the lifecycle: `active` → `suspended` → `active`, and `suspended` → `deleted`
4. Publish `platform.tenant.provisioned.v1`, `.updated.v1`, `.suspended.v1`, `.reinstated.v1` and `.deleted.v1` through the outbox, in the transaction that changes the record
5. Serve the system admin console

## Multi-Tenancy

- The tables are platform-wide, without RLS, like `tenant_hosts` (ADR-020 rule 4). All access is in platform transactions (`dbkit.WithPlatformTx`).
- System admins only. Gateway routes require the `system_admin` role on platform hosts, and the service refuses any other caller.
- Events are published on the `_platform` tenant; the acting system admin is the envelope actor.

## Data

| Table | Purpose |
|---|---|
| `tenants` | One row per shop. `tenant_id` and `store_currency` are immutable. `status` is `active`, `suspended` or `deleted`. `data_purged_at` stays null until the deletion cascade exists (backlog-v2). |
| `tenant_hostnames` | A shop's hostnames. `hostname` is the primary key; a partial unique index allows exactly one `is_primary` per tenant. |
| `idempotency_keys` | `Idempotency-Key` storage (api-conventions §10). |

A deleted tenant's hostnames are released (rows removed) so they can be given to a new tenant. The `tenant_id` is never reused.

## Lifecycle

| From | Action | To | Rules |
|---|---|---|---|
| — | create | `active` | `tenant_id` and hostnames must be free |
| `active` | suspend | `suspended` | `reason` required (1–500 characters) |
| `suspended` | reinstate | `active` | |
| `suspended` | delete | `deleted` | `confirm_tenant_id` must repeat the ID |

Any other transition is `409`. A deleted tenant cannot be changed.

## API

All paths are on platform hosts; system admins only.

| Method | Path | Description |
|---|---|---|
| GET | `/sysadmin/tenants` | List; filter by `status`, search `q` by name or hostname; cursor-paginated (ordered by `tenant_id`) |
| POST | `/sysadmin/tenants` | Create; body as `platform.tenant.provisioned.v1` plus `contact_email`; `Idempotency-Key` required |
| GET | `/sysadmin/tenants/{id}` | Read |
| PATCH | `/sysadmin/tenants/{id}` | Change `name`, `contact_email`, optional `vat_number` / `invoice_address` |
| PUT | `/sysadmin/tenants/{id}/hostnames` | Replace hostnames and the primary |
| POST | `/sysadmin/tenants/{id}/suspend` | `reason` required; `Idempotency-Key` required |
| POST | `/sysadmin/tenants/{id}/reinstate` | `Idempotency-Key` required |
| DELETE | `/sysadmin/tenants/{id}` | Only when suspended; body `confirm_tenant_id` must repeat the ID |
| GET | `/admin/shop` | Own shop invoice identity (shop_admin) |
| PATCH | `/admin/shop` | Update `vat_number` and/or `invoice_address` (shop_admin) |
| GET | `/internal/tenants/{id}` | Invoice snapshot for service callers (issue #29) |

Errors: `400` validation, `403` not a system admin, `404` unknown tenant, `409` duplicate `tenant_id`, hostname already used (by another tenant or as a platform host) or invalid state transition, `422` reused `Idempotency-Key` with another body.

The full contract is `services/yc-tenant/api/openapi.yaml`.

## Events Published

All on `_platform` through the outbox, in the same transaction as the change.

| Event | When |
|---|---|
| `platform.tenant.provisioned.v1` | Create; `hostnames[0]` is the primary |
| `platform.tenant.updated.v1` | Name or hostnames (or primary) changed; carries the full set after the change |
| `platform.tenant.suspended.v1` | Suspend, with the reason |
| `platform.tenant.reinstated.v1` | Reinstate |
| `platform.tenant.deleted.v1` | Delete |

Changing only `contact_email` publishes nothing.

## Events Consumed

None in v1.1.

## Configuration

| Key | Purpose |
|---|---|
| `platform_hosts` | Hostnames that resolve to `_platform` (mirror of yc-gateway's `platform_hosts`, `YC_TENANT_PLATFORM_HOSTS`); never allowed as a tenant hostname |
| `database.url` | `YC_TENANT_DB_URL`; `database.migrate_on_start` |
| `nats.url` | `NATS_URL` |
| `gateway.*` | Self-registration with yc-gateway (ADR-020 rule 7) |
| `idempotency.retention`, `idempotency.purge_interval` | Idempotency key retention (at least 24 h) |

## Not In Scope

- Purging a deleted tenant's data across services (backlog-v2 P2)
- Per-shop checkout configuration, CORS, branding fields
- Changing jurisdiction or tax-inclusive (owned by the services' admin APIs, ADR-029 rule 2)
