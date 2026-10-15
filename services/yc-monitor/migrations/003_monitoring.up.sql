-- Phase 8: service health, counters, thresholds and alerts
-- (docs/services/yc-monitor.md, ADR-025).

-- Services registered with yc-gateway, health-checked at base_url/readyz.
-- Platform-only: no tenant, read and written in the _platform context.
CREATE TABLE services (
    name                  TEXT PRIMARY KEY,
    instance_id           TEXT NOT NULL,
    base_url              TEXT NOT NULL,
    version               TEXT,
    status                TEXT NOT NULL DEFAULT 'unknown'
                          CHECK (status IN ('unknown', 'healthy', 'unhealthy', 'deregistered')),
    consecutive_failures  INT NOT NULL DEFAULT 0,
    consecutive_successes INT NOT NULL DEFAULT 0,
    last_error            TEXT,
    last_check_at         TIMESTAMPTZ,
    last_healthy_at       TIMESTAMPTZ,
    unhealthy_since       TIMESTAMPTZ,
    registered_at         TIMESTAMPTZ NOT NULL,
    deregistered_at       TIMESTAMPTZ
);

-- Counters rolled up into 1m, 5m, 1h and 1d buckets. dimensions is '{}'
-- when the counter has none.
CREATE TABLE metric_rollups (
    tenant_id    TEXT NOT NULL,
    metric       TEXT NOT NULL,
    bucket_size  TEXT NOT NULL CHECK (bucket_size IN ('1m', '5m', '1h', '1d')),
    bucket_start TIMESTAMPTZ NOT NULL,
    dimensions   JSONB NOT NULL DEFAULT '{}',
    value        BIGINT NOT NULL,
    PRIMARY KEY (tenant_id, metric, bucket_size, bucket_start, dimensions)
);
CREATE INDEX idx_metric_rollups_time ON metric_rollups (metric, bucket_size, bucket_start DESC);

-- Each counted event, so a redelivery is not counted twice.
CREATE TABLE counted_events (
    tenant_id  TEXT NOT NULL,
    event_id   UUID PRIMARY KEY,
    counted_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_counted_events_time ON counted_events (counted_at);

-- Overrides of the built-in alert rules' thresholds. _platform rows apply
-- to every shop; a shop's row overrides them for that shop.
CREATE TABLE alert_thresholds (
    tenant_id      TEXT NOT NULL,
    rule           TEXT NOT NULL,
    threshold      DOUBLE PRECISION NOT NULL,
    min_volume     BIGINT NOT NULL DEFAULT 0,
    window_minutes INT NOT NULL CHECK (window_minutes BETWEEN 1 AND 1440),
    enabled        BOOLEAN NOT NULL DEFAULT TRUE,
    updated_by     TEXT NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, rule)
);

-- scope is the alert's dimensions as a stable string, so there is at most
-- one unresolved alert per rule and scope in a tenant.
CREATE TABLE alerts (
    id              UUID PRIMARY KEY,
    tenant_id       TEXT NOT NULL,
    rule            TEXT NOT NULL,
    metric          TEXT NOT NULL,
    scope           TEXT NOT NULL,
    dimensions      JSONB NOT NULL DEFAULT '{}',
    severity        TEXT NOT NULL CHECK (severity IN ('info', 'warning', 'critical')),
    summary         TEXT NOT NULL,
    value           DOUBLE PRECISION,
    threshold       DOUBLE PRECISION,
    notify          TEXT[] NOT NULL DEFAULT '{}',
    occurrences     INT NOT NULL DEFAULT 1,
    opened_at       TIMESTAMPTZ NOT NULL,
    last_seen_at    TIMESTAMPTZ NOT NULL,
    acknowledged_at TIMESTAMPTZ,
    acknowledged_by TEXT,
    resolved_at     TIMESTAMPTZ
);
CREATE UNIQUE INDEX idx_alerts_active ON alerts (tenant_id, rule, scope) WHERE resolved_at IS NULL;
CREATE INDEX idx_alerts_tenant ON alerts (tenant_id, opened_at DESC);

CREATE INDEX idx_dlq_entries_created ON dlq_entries (created_at);

ALTER TABLE metric_rollups ENABLE ROW LEVEL SECURITY;
ALTER TABLE metric_rollups FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON metric_rollups
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform')
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE counted_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE counted_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON counted_events
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform')
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE alert_thresholds ENABLE ROW LEVEL SECURITY;
ALTER TABLE alert_thresholds FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON alert_thresholds
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform')
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)
                OR current_setting('app.tenant_id', true) = '_platform');

ALTER TABLE alerts ENABLE ROW LEVEL SECURITY;
ALTER TABLE alerts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON alerts
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform')
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)
                OR current_setting('app.tenant_id', true) = '_platform');
