-- Suspended tenants and freed-but-gone hostnames (ADR-029 rules 5–6).
-- Loaded at startup so a restart keeps 503/410 answers.
CREATE TABLE suspended_tenants (
    tenant_id  TEXT PRIMARY KEY,
    reason     TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE deleted_hosts (
    hostname   TEXT PRIMARY KEY CHECK (hostname = lower(hostname)),
    tenant_id  TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_deleted_hosts_tenant ON deleted_hosts (tenant_id);
