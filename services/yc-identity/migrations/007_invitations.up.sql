-- Admin invitations (ADR-030), shop primary-hostname projection for accept
-- links, and Idempotency-Key storage for POST /sysadmin/invitations.
CREATE TABLE invitations (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        TEXT NOT NULL,
    email            TEXT NOT NULL,
    role             TEXT NOT NULL CHECK (role IN ('shop_admin', 'system_admin')),
    status           TEXT NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending', 'accepted', 'revoked', 'expired')),
    display_name     TEXT,
    token_hash       BYTEA UNIQUE,           -- SHA-256; NULL once accepted/revoked
    expires_at       TIMESTAMPTZ NOT NULL,
    invited_by       UUID NOT NULL,
    accepted_user_id UUID REFERENCES users(id),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at       TIMESTAMPTZ,
    CHECK ((role = 'system_admin') = (tenant_id = '_platform'))
);
CREATE INDEX idx_invitations_tenant ON invitations (tenant_id, created_at DESC);
CREATE INDEX idx_invitations_tenant_status ON invitations (tenant_id, status);
CREATE UNIQUE INDEX idx_invitations_pending_email
    ON invitations (tenant_id, email) WHERE status = 'pending';

CREATE TABLE shop_hosts (
    tenant_id         TEXT PRIMARY KEY,
    primary_hostname  TEXT NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL
);

CREATE TABLE idempotency_keys (
    scope        TEXT NOT NULL,
    key          TEXT NOT NULL,
    request_hash BYTEA NOT NULL,
    status       INT,
    content_type TEXT,
    body         BYTEA,
    locked_until TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ,
    PRIMARY KEY (scope, key)
);
CREATE INDEX idx_idempotency_keys_created ON idempotency_keys (created_at);

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['invitations', 'shop_hosts'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING (tenant_id = current_setting('app.tenant_id', true)
                   OR current_setting('app.tenant_id', true) = '_platform')
            WITH CHECK (tenant_id = current_setting('app.tenant_id', true))$p$, t);
    END LOOP;
END $$;
