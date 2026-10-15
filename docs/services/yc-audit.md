# yc-audit — Service Requirement

## Overview

`yc-audit` is the platform's audit log. It subscribes to admin actions, security events, GDPR cascade events, and configuration changes across all services, and stores an immutable record. It is the authoritative compliance and forensic log.

## Location in Monorepo

```
/services/yc-audit/
```

## Responsibilities

1. Subscribe to audit-worthy events across the platform
2. Store an immutable audit log
3. Track GDPR cascade completion per deletion request
4. Raise compliance incidents for incomplete cascades
5. Expose a read-only API for system admins (and limited scope for shop admins)
6. Retain records per retention policy (ADR-004: infinite / 7 years + cold archive)
7. Provide export tooling for audits and compliance reviews

## Multi-Tenancy

- Audit records are tenant-scoped (except `_platform` records)
- Shop admins see only their tenant's audit records; limited to their own domain of concern (products, orders, prices they changed)
- System admins see all audit records including `_platform`

## Immutability

- Audit records are **append-only**
- No update or delete API, ever
- Table has no `updated_at`; database user has `INSERT` and `SELECT` only; no `UPDATE` or `DELETE` grant
- Enforced at the database privilege level, not just application logic

## Record Categories

| Category | Source |
|---|---|
| `admin_action` | Any `*.admin_action.v1` event from any service |
| `role_change` | `identity.user.promoted.v1`, `identity.user.demoted.v1` |
| `tenant_lifecycle` | `platform.tenant.*` |
| `security` | Login failures (above threshold), webhook signature failures |
| `config_change` | `platform.tenant.configuration_changed.v1`, `pricing.config.changed.v1`, `*.provider_config_changed.v1` |
| `gdpr` | `identity.user.deleted.v1`, `platform.gdpr.cascade_completed.v1` |
| `financial` | `payment.chargeback_received.v1`, manual refunds |

## GDPR Cascade Register

A dedicated table tracks each deletion:

```
cascade_register
  ├── user_id
  ├── pseudonym
  ├── initiated_at
  ├── expected_services[]      # from a configured list
  ├── completed_services[]     # populated as events arrive
  └── status                    # 'in_progress','complete','incident'
```

- When `identity.user.deleted.v1` arrives: create register entry
- When `platform.gdpr.cascade_completed.v1` arrives: mark service complete
- If `complete` not reached by `initiated_at + 72h`: transition to `incident`, publish `audit.gdpr.cascade_incident.v1`, which triggers yc-message to alert system admin

## API

### System Admin Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| GET | `/sysadmin/audit` | List audit records (filter by category, tenant, time) | system_admin |
| GET | `/sysadmin/audit/{id}` | Record detail | system_admin |
| GET | `/sysadmin/audit/export` | Export records (CSV, JSONL) | system_admin |
| GET | `/sysadmin/gdpr/register` | List cascade registrations | system_admin |
| GET | `/sysadmin/gdpr/register/{user_id}` | One user's cascade: status, expected and completed services | system_admin |
| GET | `/sysadmin/gdpr/incidents` | List incidents | system_admin |

### Shop Admin Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| GET | `/admin/audit` | Tenant-scoped audit records | shop_admin |

### Phase 8 Implementation Notes

The contract is `services/yc-audit/api/openapi.yaml`; the design is ADR-025.

- **Recorded events** (one record per event ID): `identity.user.promoted.v1` (role change); `platform.tenant.provisioned.v1` (tenant lifecycle); `pricing.config.changed.v1` (config change); `identity.user.deleted.v1`, `platform.gdpr.cascade_completed.v1`, `audit.gdpr.cascade_complete.v1` and `.cascade_incident.v1` (GDPR); `payment.chargeback_received.v1`, `order.disputed.v1` and `order.refunded.v1` (financial); `payment.webhook_signature_failed.v1` (security). There is no `admin_action` category; demotions, login failures and tenant configuration events come when those events exist.
- **Immutability:** `UPDATE`, `DELETE` and `TRUNCATE` are revoked from `PUBLIC` and from the migrating role, and triggers refuse them for anyone.
- **Filters:** `category`, `tenant_id` (system admins), `event_type`, `subject_ref` (`user:<id>`, `order:<id>`, `transaction:<id>` …), `from`, `to`, with cursor paging. `GET /sysadmin/audit/export?format=csv|jsonl` returns at most `export.max_records` (10,000).
- **Register:** the configured `gdpr.expected_services` are copied into each entry; reports that arrive first are kept in `gdpr_cascade_reports` until the deletion does. `GET /sysadmin/gdpr/register?status=` lists entries; `/{user_id}` adds each service's report. Every `sweep_interval` (1m) entries past `deadline` (72h, `YC_AUDIT_GDPR_DEADLINE`) become incidents.

## Database Schema

Database: **`yc_audit`**

```sql
CREATE TABLE audit_records (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       TEXT NOT NULL,
    category        TEXT NOT NULL,
    event_type      TEXT NOT NULL,
    event_id        UUID NOT NULL,
    source_service  TEXT NOT NULL,
    actor_id        TEXT,
    actor_type      TEXT,
    subject_ref     TEXT,                   -- what was changed (e.g., "product:<id>")
    payload         JSONB NOT NULL,
    event_time      TIMESTAMPTZ NOT NULL,
    recorded_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (event_id)
);
CREATE INDEX idx_audit_tenant_time ON audit_records (tenant_id, event_time DESC);
CREATE INDEX idx_audit_category ON audit_records (category, event_time DESC);
CREATE INDEX idx_audit_subject ON audit_records (tenant_id, subject_ref);

CREATE TABLE gdpr_cascade_register (
    user_id             UUID PRIMARY KEY,
    pseudonym           TEXT NOT NULL UNIQUE,
    tenant_id           TEXT NOT NULL,
    initiated_at        TIMESTAMPTZ NOT NULL,
    expected_services   TEXT[] NOT NULL,
    completed_services  TEXT[] NOT NULL DEFAULT '{}',
    status              TEXT NOT NULL DEFAULT 'in_progress',  -- 'in_progress','complete','incident'
    completed_at        TIMESTAMPTZ,
    incident_at         TIMESTAMPTZ
);

CREATE TABLE outbox ( ... );
CREATE TABLE processed_events ( ... );

-- audit_records is INSERT-only
REVOKE UPDATE, DELETE ON audit_records FROM yc_audit_app;
```

## Published Events

| Event | When |
|---|---|
| `audit.record.stored.v1` | Any audit record stored (used by yc-monitor for throughput metrics) |
| `audit.gdpr.cascade_complete.v1` | All services reported |
| `audit.gdpr.cascade_incident.v1` | 72h window exceeded |

## Subscribed Events

A wide fan-in. Representative list:

| Event | Action |
|---|---|
| `*.admin_action.v1` (any service) | Store as `admin_action` |
| `identity.user.promoted.v1` | Store as `role_change` |
| `identity.user.demoted.v1` | Store as `role_change` |
| `identity.user.suspended.v1` | Store as `security` |
| `identity.user.login_failed.v1` | Aggregate; store as `security` if threshold exceeded per actor/period |
| `identity.user.deleted.v1` | Open cascade register entry |
| `platform.gdpr.cascade_completed.v1` | Update cascade register |
| `platform.tenant.*` | Store as `tenant_lifecycle` |
| `platform.tenant.configuration_changed.v1` | Store as `config_change` |
| `pricing.config.changed.v1` | Store as `config_change` |
| `payment.chargeback_received.v1` | Store as `financial` |
| `payment.webhook_signature_failed.v1` | Store as `security` |

## Configuration

```yaml
yc-audit:
  listen: :8080
  database:
    url: postgres://yc_audit:...@...
  gdpr:
    expected_services:
      - yc-identity
      - yc-preference
      - yc-cart
      - yc-review
      - yc-orders
      - yc-message
      - yc-inventory
      - yc-product
      - yc-checkout
      - yc-shipping
    incident_threshold_hours: 72
  cold_archive:
    enabled: false                       # future scope
    bucket: ...
    after_years: 2
  nats:
    url: nats://...
  log_level: info
```

## Dependencies on Other Services

- **None** (consume-only)

## Called By

- System admins
- Shop admins (scoped read-only)

## GDPR Behavior

- Audit records themselves are retained — the audit of what happened is proof of compliance
- When a user is deleted: audit records referencing their `user_id` are **not** modified; the GDPR cascade register and yc-identity's pseudonym mapping bridge the deletion
- An exception: in jurisdictions where even audit logs must be deleted on request, future scope would include a cryptographic shredding mechanism

## Open Questions / Future Scope

- Cold archival to object storage after N years
- Cryptographic chain (hash each record over previous) for tamper evidence
- SIEM integration (export to Splunk, Datadog, etc.)
- Audit record search with full-text
- Compliance report generation (PCI, GDPR Article 30 records)
- Role-based filtering within audit API (what shop admins see vs system admins)
