-- Phase 11: each shop's name and primary hostname, projected from
-- platform.tenant.provisioned.v1 / updated.v1, so links use the real
-- hostname (ADR-029, ADR-027 rule 14). messages.shop_url is the fallback.
CREATE TABLE shop_tenants (
    tenant_id         TEXT PRIMARY KEY,
    name              TEXT NOT NULL,
    primary_hostname  TEXT NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL
);

ALTER TABLE shop_tenants ENABLE ROW LEVEL SECURITY;
ALTER TABLE shop_tenants FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON shop_tenants
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform')
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
