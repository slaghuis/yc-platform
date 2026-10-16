-- The tenant registry (ADR-029). Platform-wide tables, without RLS, like
-- yc-gateway's tenant_hosts (ADR-020 rule 4): only system admins reach them,
-- always through platform transactions.
CREATE TABLE tenants (
    tenant_id            TEXT PRIMARY KEY,
    name                 TEXT NOT NULL,
    status               TEXT NOT NULL CHECK (status IN ('active', 'suspended', 'deleted')),
    store_currency       TEXT NOT NULL,
    default_jurisdiction TEXT NOT NULL,
    is_tax_inclusive     BOOLEAN NOT NULL,
    contact_email        TEXT NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL,
    updated_at           TIMESTAMPTZ NOT NULL,
    suspended_at         TIMESTAMPTZ,
    suspended_reason     TEXT,
    deleted_at           TIMESTAMPTZ,
    data_purged_at       TIMESTAMPTZ
);
CREATE INDEX tenants_status ON tenants (status);

CREATE TABLE tenant_hostnames (
    hostname   TEXT PRIMARY KEY,
    tenant_id  TEXT NOT NULL REFERENCES tenants (tenant_id),
    is_primary BOOLEAN NOT NULL DEFAULT false
);
-- Exactly one primary per tenant (at most one here; the service always sets one).
CREATE UNIQUE INDEX tenant_hostnames_one_primary ON tenant_hostnames (tenant_id) WHERE is_primary;
CREATE INDEX tenant_hostnames_tenant ON tenant_hostnames (tenant_id);
