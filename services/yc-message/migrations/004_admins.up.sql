-- Phase 8: each shop's admins, projected from identity.user.promoted.v1,
-- so alerts and milestones reach them (yc-identity's user API is not
-- reachable through the gateway's internal listener).
CREATE TABLE shop_admins (
    tenant_id   TEXT NOT NULL,
    user_id     TEXT NOT NULL,
    promoted_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, user_id)
);

ALTER TABLE shop_admins ENABLE ROW LEVEL SECURITY;
ALTER TABLE shop_admins FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON shop_admins
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform')
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
