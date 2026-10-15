-- The audit log and the GDPR cascade register (docs/services/yc-audit.md,
-- ADR-025).

-- One record per audited event. Append-only: the service user cannot
-- update, delete or truncate it, and triggers refuse those for anyone,
-- including a role that grants itself the privileges back.
CREATE TABLE audit_records (
    id             UUID PRIMARY KEY,
    tenant_id      TEXT NOT NULL,
    category       TEXT NOT NULL CHECK (category IN
                       ('role_change', 'tenant_lifecycle', 'config_change', 'gdpr', 'financial', 'security')),
    event_type     TEXT NOT NULL,
    event_id       UUID NOT NULL UNIQUE,
    source_service TEXT NOT NULL,
    actor_type     TEXT,
    actor_id       TEXT,
    subject_ref    TEXT,
    summary        TEXT NOT NULL,
    correlation_id TEXT,
    payload        JSONB NOT NULL,
    event_time     TIMESTAMPTZ NOT NULL,
    recorded_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_audit_time ON audit_records (event_time DESC, id DESC);
CREATE INDEX idx_audit_tenant_time ON audit_records (tenant_id, event_time DESC, id DESC);
CREATE INDEX idx_audit_category ON audit_records (category, event_time DESC);
CREATE INDEX idx_audit_subject ON audit_records (tenant_id, subject_ref);

CREATE FUNCTION audit_records_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_records is append-only' USING ERRCODE = 'insufficient_privilege';
END $$;
CREATE TRIGGER audit_records_no_change BEFORE UPDATE OR DELETE ON audit_records
    FOR EACH ROW EXECUTE FUNCTION audit_records_append_only();
CREATE TRIGGER audit_records_no_truncate BEFORE TRUNCATE ON audit_records
    FOR EACH STATEMENT EXECUTE FUNCTION audit_records_append_only();
REVOKE UPDATE, DELETE, TRUNCATE ON audit_records FROM PUBLIC;
REVOKE UPDATE, DELETE, TRUNCATE ON audit_records FROM CURRENT_USER;

-- One GDPR deletion (ADR-005).
CREATE TABLE gdpr_cascade_register (
    tenant_id         TEXT NOT NULL,
    user_id           TEXT NOT NULL,
    pseudonym         TEXT NOT NULL UNIQUE,
    role              TEXT NOT NULL,
    requested_by      TEXT NOT NULL,
    initiated_at      TIMESTAMPTZ NOT NULL,
    deadline          TIMESTAMPTZ NOT NULL,
    expected_services TEXT[] NOT NULL,
    status            TEXT NOT NULL DEFAULT 'in_progress' CHECK (status IN ('in_progress', 'complete', 'incident')),
    completed_at      TIMESTAMPTZ,
    incident_at       TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, user_id)
);
CREATE INDEX idx_register_open ON gdpr_cascade_register (deadline) WHERE status = 'in_progress';
CREATE INDEX idx_register_list ON gdpr_cascade_register (initiated_at DESC, user_id DESC);

-- Each service's report. Kept apart from the register because a report
-- can arrive before the deletion event that opens it.
CREATE TABLE gdpr_cascade_reports (
    tenant_id    TEXT NOT NULL,
    user_id      TEXT NOT NULL,
    service      TEXT NOT NULL,
    action       TEXT NOT NULL,
    record_count BIGINT NOT NULL,
    completed_at TIMESTAMPTZ NOT NULL,
    reported_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, user_id, service)
);

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['audit_records', 'gdpr_cascade_register', 'gdpr_cascade_reports'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING (tenant_id = current_setting('app.tenant_id', true)
                   OR current_setting('app.tenant_id', true) = '_platform')
            WITH CHECK (tenant_id = current_setting('app.tenant_id', true))$p$, t);
    END LOOP;
END $$;
