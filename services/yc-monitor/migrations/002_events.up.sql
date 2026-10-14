-- Phase 1 stub: every consumed event, so the demo can prove the chain.
CREATE TABLE received_events (
    event_id        UUID PRIMARY KEY,
    tenant_id       TEXT NOT NULL,
    event_type      TEXT NOT NULL,
    source_service  TEXT NOT NULL,
    correlation_id  TEXT,
    event_time      TIMESTAMPTZ NOT NULL,
    envelope        JSONB NOT NULL,
    received_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_received_events_tenant ON received_events (tenant_id, received_at DESC);

-- docs/services/yc-monitor.md, plus dlq_sequence: an event can be
-- dead-lettered more than once, so the YC_DLQ stream sequence identifies
-- the dead letter.
CREATE TABLE dlq_entries (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    dlq_sequence     BIGINT NOT NULL UNIQUE,
    tenant_id        TEXT NOT NULL,
    service          TEXT NOT NULL,
    consumer         TEXT NOT NULL,
    event_type       TEXT NOT NULL,
    event_id         UUID NOT NULL,
    failure_reason   TEXT NOT NULL,
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

-- The service user owns the tables, so FORCE is needed for RLS to apply.
ALTER TABLE received_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE received_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON received_events
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform')
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE dlq_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE dlq_entries FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON dlq_entries
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform')
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
