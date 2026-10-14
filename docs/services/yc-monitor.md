# yc-monitor — Service Requirement

## Overview

`yc-monitor` is the operations service. It tracks service health, aggregates DLQ activity, monitors throughput, and raises alerts on anomalies. It is **not** a replacement for infrastructure-level monitoring (Prometheus, Grafana) — it complements them with platform-aware intelligence (per-tenant metrics, business-level anomalies, cascade incidents).

## Location in Monorepo

```
/services/yc-monitor/
```

## Responsibilities

1. Perform health checks on every registered service
2. Aggregate DLQ activity per tenant and per service
3. Monitor throughput per event type, per tenant, per service
4. Detect anomalies (sudden drop in checkouts, surge in failures)
5. Receive and route critical events (chargebacks, cascade incidents, service down)
6. Expose a dashboard API for system admins
7. Push alerts to configured channels (via yc-message or external PagerDuty/Slack)
8. Track service registration lifecycle

## Multi-Tenancy

- Metrics are aggregated per tenant where tenant-scoped
- System-level metrics are under `_platform`
- Alerts can be tenant-scoped (routed to shop admin) or platform-scoped (routed to system admin)

## Health Check Model

- Every service exposes `/healthz` (liveness) and `/readyz` (readiness)
- yc-monitor polls `/readyz` every 15 seconds (configurable)
- Three consecutive failures (45 seconds) → service marked `unhealthy`
- Three consecutive successes after unhealthy → `recovered`
- Also receives yc-gateway's `platform.service.registered.v1` / `deregistered.v1` events for lifecycle

## Metrics Collected

| Metric | Dimensions |
|---|---|
| `service.up` | service, instance |
| `service.response_time_ms` | service, endpoint |
| `events.published` | tenant, service, event_type |
| `events.consumed` | tenant, service, event_type |
| `events.dlq` | tenant, service |
| `events.processing_time_ms` | tenant, service, event_type |
| `checkouts.started` | tenant |
| `checkouts.completed` | tenant |
| `checkouts.failed` | tenant |
| `orders.placed` | tenant |
| `payments.failed` | tenant, gateway |

- Metrics are rolled up into time buckets (1 min, 5 min, 1 hour, 1 day)
- Raw metrics retained 30 days; rollups retained longer

## Anomaly Detection (v1 — simple)

- Rate-based thresholds (configurable per metric per tenant)
  - "DLQ rate > 10 messages/minute → alert"
  - "Checkout success rate < 50% over 5 min with volume > N → alert"
- No ML in v1 — just configured thresholds
- Future: baseline-based detection

## Alerting

- Alerts have severity: `info`, `warning`, `critical`
- Routing per severity, per tenant, configurable:
  - To yc-message (email, SMS to admin)
  - To external webhook (Slack, PagerDuty, OpsGenie)
- Deduplication: an active alert for the same `(tenant, metric, dimensions)` is updated, not re-raised

## API

### System Admin Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| GET | `/sysadmin/services` | List services and health | system_admin |
| GET | `/sysadmin/metrics` | Query metrics | system_admin |
| GET | `/sysadmin/dlq` | DLQ summary across all tenants | system_admin |
| GET | `/sysadmin/alerts` | List alerts | system_admin |
| POST | `/sysadmin/alerts/{id}/ack` | Acknowledge alert | system_admin |
| PUT | `/sysadmin/thresholds` | Configure thresholds | system_admin |

### Shop Admin Endpoints

| Method | Path | Description | Auth |
|---|---|---|---|
| GET | `/admin/metrics` | Tenant-scoped metrics | shop_admin |
| GET | `/admin/dlq` | Tenant DLQ summary | shop_admin |
| GET | `/admin/alerts` | Tenant alerts | shop_admin |

### Internal Endpoints

Standard `/healthz` and `/readyz` on every service — consumed by yc-monitor.

## Database Schema

Database: **`yc_monitor`**

```sql
CREATE TABLE services (
    name                 TEXT PRIMARY KEY,
    base_url             TEXT NOT NULL,
    health_url           TEXT NOT NULL,
    status               TEXT NOT NULL,          -- 'healthy','unhealthy','unknown'
    last_check_at        TIMESTAMPTZ,
    last_healthy_at      TIMESTAMPTZ,
    consecutive_failures INT NOT NULL DEFAULT 0,
    registered_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE metric_rollups (
    bucket_start    TIMESTAMPTZ NOT NULL,
    bucket_size     TEXT NOT NULL,              -- '1m','5m','1h','1d'
    metric          TEXT NOT NULL,
    tenant_id       TEXT NOT NULL,
    dimensions      JSONB NOT NULL DEFAULT '{}',
    value           NUMERIC NOT NULL,
    count           BIGINT NOT NULL DEFAULT 1,
    PRIMARY KEY (bucket_start, bucket_size, metric, tenant_id, dimensions)
);
CREATE INDEX idx_metric_lookup ON metric_rollups (metric, tenant_id, bucket_start DESC);

CREATE TABLE dlq_entries (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        TEXT NOT NULL,
    service          TEXT NOT NULL,
    event_type       TEXT NOT NULL,
    event_id         UUID NOT NULL,
    attempts         INT NOT NULL,
    last_error       TEXT,
    first_failure_at TIMESTAMPTZ,
    last_failure_at  TIMESTAMPTZ,
    envelope         JSONB NOT NULL,
    acknowledged     BOOLEAN NOT NULL DEFAULT FALSE,
    acknowledged_by  TEXT,
    acknowledged_at  TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_dlq_tenant_service ON dlq_entries (tenant_id, service, acknowledged, created_at DESC);

CREATE TABLE alert_rules (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       TEXT NOT NULL,              -- '_platform' for cross-tenant rules
    name            TEXT NOT NULL,
    metric          TEXT NOT NULL,
    dimensions      JSONB NOT NULL DEFAULT '{}',
    comparator      TEXT NOT NULL,              -- 'gt','lt','gte','lte','eq'
    threshold       NUMERIC NOT NULL,
    window_minutes  INT NOT NULL,
    min_volume      BIGINT NOT NULL DEFAULT 0,  -- suppress alerts under this volume
    severity        TEXT NOT NULL,              -- 'info','warning','critical'
    routing         JSONB NOT NULL,             -- channels per severity
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE alerts (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    rule_id         UUID REFERENCES alert_rules(id),
    tenant_id       TEXT NOT NULL,
    metric          TEXT NOT NULL,
    dimensions      JSONB NOT NULL DEFAULT '{}',
    severity        TEXT NOT NULL,
    opened_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at     TIMESTAMPTZ,
    acknowledged_at TIMESTAMPTZ,
    acknowledged_by TEXT,
    current_value   NUMERIC,
    message         TEXT,
    UNIQUE (rule_id, tenant_id, dimensions, resolved_at)  -- one active alert per rule+scope
);
CREATE INDEX idx_alerts_open ON alerts (tenant_id, opened_at DESC) WHERE resolved_at IS NULL;

CREATE TABLE outbox ( ... );
CREATE TABLE processed_events ( ... );

ALTER TABLE metric_rollups ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON metric_rollups
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform');
-- similar on dlq_entries, alerts, alert_rules
```

## Published Events

| Event | When |
|---|---|
| `monitor.service.unhealthy.v1` | Service failed health checks |
| `monitor.service.recovered.v1` | Service recovered |
| `monitor.alert.opened.v1` | New alert raised |
| `monitor.alert.resolved.v1` | Alert condition cleared |
| `monitor.alert.acknowledged.v1` | Admin acknowledged |
| `monitor.dlq.spike_detected.v1` | DLQ throughput exceeded threshold |
| `monitor.threshold.breached.v1` | Generic threshold breach |

## Subscribed Events

A broad fan-in. Representative list:

| Event | Action |
|---|---|
| `platform.service.registered.v1` | Register service for health checks |
| `platform.service.deregistered.v1` | Stop health checks for the service |
| `yc.*.*.dlq.v1` (wildcard across all tenants) | Record DLQ entry; raise alert if threshold breached |
| `checkout.started.v1` | Increment metric |
| `checkout.completed.v1` | Increment metric; recompute success rate |
| `checkout.failed.v1` | Increment metric |
| `order.placed.v1` | Increment metric |
| `payment.authorization_failed.v1` | Increment metric; raise alert on surge |
| `payment.charge_failed.v1` | Same |
| `payment.capture_failed.v1` | Same |
| `payment.webhook_signature_failed.v1` | Record security event; raise alert |
| `audit.gdpr.cascade_incident.v1` | Raise critical alert — compliance incident |
| `pricing.exchange_rates_stale.v1` | Raise warning alert |

## Configuration

```yaml
yc-monitor:
  listen: :8080
  database:
    url: postgres://yc_monitor:...@...
  health_check:
    interval_seconds: 15
    failures_to_unhealthy: 3
    successes_to_recovered: 3
  rollup:
    buckets:
      - 1m
      - 5m
      - 1h
      - 1d
    retention:
      raw_days: 30
      1m_days: 7
      5m_days: 30
      1h_days: 365
      1d_days: 1825
  default_thresholds:
    dlq_per_minute: 10
    checkout_success_rate_min: 0.5
    checkout_success_rate_min_volume: 10
  routing:
    critical:
      - channel: yc-message
        audience: system_admin
      - channel: webhook
        url_env: YC_MONITOR_CRITICAL_WEBHOOK
    warning:
      - channel: yc-message
        audience: system_admin
    info:
      - channel: log
  nats:
    url: nats://...
  log_level: info
```

## Dependencies on Other Services

- **yc-gateway** (service registration events)
- **yc-message** (alert delivery)
- External: optional PagerDuty/Slack webhooks

## Called By

- System admins (dashboard)
- Shop admins (tenant-scoped dashboard)
- All services (healthchecks are called by yc-monitor, not vice versa)

## GDPR Behavior

- Metrics are aggregate; no PII
- DLQ entries may contain PII in the embedded original event payload — treated as sensitive
- On `identity.user.deleted.v1`: no cascade subscription (no per-user data stored)
- DLQ entries older than 90 days are purged per ADR-004

## Open Questions / Future Scope

- Infrastructure metrics (CPU, memory) via Prometheus scraping
- Full tracing dashboard (OpenTelemetry integration)
- ML-based anomaly detection
- SLO tracking with error budgets
- Status page generation (public uptime page per tenant)
- Dependency graph visualization
- Capacity forecasting
