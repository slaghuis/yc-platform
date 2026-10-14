-- Hostname → tenant map (ADR-020). Platform-wide: read before a tenant is
-- known, so it has no RLS.
CREATE TABLE tenant_hosts (
    hostname   TEXT PRIMARY KEY CHECK (hostname = lower(hostname)),  -- no port
    tenant_id  TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_tenant_hosts_tenant ON tenant_hosts (tenant_id);
