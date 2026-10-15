-- GDPR deletion (ADR-005). The pseudonym replaces a deleted user's ID in the
-- records other services keep; this table is the only way back from it and
-- is under legal hold: no endpoint reads it.
CREATE TABLE pseudonym_map (
    user_id      UUID PRIMARY KEY,
    tenant_id    TEXT NOT NULL,
    pseudonym    UUID NOT NULL UNIQUE,
    requested_by TEXT NOT NULL CHECK (requested_by IN ('user', 'system_admin')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE pseudonym_map ENABLE ROW LEVEL SECURITY;
ALTER TABLE pseudonym_map FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON pseudonym_map
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform')
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
