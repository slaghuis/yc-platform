# ADR-017: Database Per Service

**Status:** Accepted
**Date:** 2025-01-15
**Context:** Microservices should own their data. Shared databases create hidden coupling, make independent deployment impossible, and defeat the purpose of service boundaries.

## Decision

**Every stateful service has its own Postgres database.** No shared databases. No cross-service SQL joins. No reading another service's tables.

## Rules

1. Each service owns a dedicated Postgres database with its own schema, migrations, and credentials
2. Services communicate with each other **only via APIs or events** — never via direct database access
3. **No foreign keys across service boundaries** — references to entities in other services are plain text IDs (UUIDs)
4. **Multi-tenancy within a service**: use `tenant_id` as a column on every row, with Postgres Row-Level Security (RLS) policies enforcing isolation. Alternative (schema-per-tenant) is **rejected** — operational cost scales poorly with tenant count.
5. **Migrations** are owned by the service: each service's repo contains its migrations (we'll standardize on `golang-migrate` or `goose`)
6. **Backups** are per-database
7. **Database credentials** are managed per service (secrets management)
8. **The outbox table (ADR-006) lives in each service's own database** — this is what makes the transactional outbox work

## Shared Infrastructure

- All databases run on the same Postgres cluster (or managed Postgres service) for operational simplicity
- Each service connects as its own user with access only to its own database
- Connection pooling (PgBouncer) is per-database or shared with appropriate pool-mode per user

## Tenant Isolation within a Database

For every service with tenant-scoped data:
- Every table has a `tenant_id` column (except truly platform-wide tables)
- RLS policies enforce `tenant_id = current_setting('app.tenant_id')`
- The service sets `app.tenant_id` on each session/transaction based on the authenticated request

This provides **defense in depth**: even an application bug that forgets to filter by tenant cannot leak data across tenants.

## Consequences

**Positive**
- True service autonomy — each service can be deployed, scaled, migrated independently
- Clear boundaries enforced by access control, not just discipline
- Each service can choose its own schema evolution pace
- Row-level security provides defense in depth against application bugs

**Negative**
- Operations manages many databases (mitigated by shared cluster)
- No cross-service queries — reporting requires an analytics pipeline (YC-Analytics addresses this via event-sourced projections)
- Data denormalization — the same conceptual entity (e.g., product name) appears in several service databases as snapshots

## Service-to-Database Map (v1)

*Amended 2026-10-14: yc-gateway owns `yc_gateway` (ADR-020).*
*Amended 2026-10-28: yc-support owns `yc_support` (issue #33).*

| Service | Database |
|---|---|
| YC-Identity | `yc_identity` |
| YC-Gateway | `yc_gateway` (tenant hostname map and outbox only; ADR-020) |
| YC-Image | `yc_image` (metadata only; images in object storage) |
| YC-Product | `yc_product` |
| YC-Pricing | `yc_pricing` |
| YC-Inventory | `yc_inventory` |
| YC-Cart | `yc_cart` |
| YC-Preference | `yc_preference` |
| YC-Review | `yc_review` |
| YC-Checkout | `yc_checkout` |
| YC-Payment | `yc_payment` |
| YC-Orders | `yc_orders` |
| YC-Shipping | `yc_shipping` |
| YC-Message | `yc_message` |
| YC-Audit | `yc_audit` |
| YC-Monitor | `yc_monitor` |
| YC-Analytics | `yc_analytics` |
| YC-Support | `yc_support` |
