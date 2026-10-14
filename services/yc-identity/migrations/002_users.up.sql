CREATE TABLE users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     TEXT NOT NULL,
    email         TEXT,                    -- NULL for visitors
    phone_e164    TEXT,
    password_hash TEXT,                    -- NULL for OAuth-only users and visitors
    role          TEXT NOT NULL CHECK (role IN ('visitor', 'customer', 'shop_admin', 'system_admin')),
    status        TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended', 'deleted')),
    display_name  TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at    TIMESTAMPTZ,
    UNIQUE (tenant_id, email),
    -- System admins are platform-wide; everyone else belongs to a shop.
    CHECK ((role = 'system_admin') = (tenant_id = '_platform'))
);
CREATE INDEX idx_users_tenant ON users (tenant_id);

-- The service user owns the table, so FORCE is needed for RLS to apply to it.
ALTER TABLE users ENABLE ROW LEVEL SECURITY;
ALTER TABLE users FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON users
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform')
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
