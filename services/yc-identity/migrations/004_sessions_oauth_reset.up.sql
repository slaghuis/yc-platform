-- Refresh tokens rotate on every use. A family is one login's chain;
-- presenting a replaced or revoked token revokes the whole family.
CREATE TABLE refresh_tokens (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   TEXT NOT NULL,
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    family_id   UUID NOT NULL,
    token_hash  BYTEA NOT NULL UNIQUE,     -- SHA-256 of the opaque token
    issued_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at  TIMESTAMPTZ NOT NULL,
    revoked_at  TIMESTAMPTZ,
    replaced_by UUID
);
CREATE INDEX idx_refresh_tokens_user ON refresh_tokens (user_id);
CREATE INDEX idx_refresh_tokens_family ON refresh_tokens (family_id);

-- Access tokens revoked by logout before they expire.
CREATE TABLE revoked_access_tokens (
    jti        UUID PRIMARY KEY,
    tenant_id  TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);

-- The same provider account may sign in to several shops, each its own
-- user, so a link is unique per tenant.
CREATE TABLE oauth_identities (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     TEXT NOT NULL,
    user_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider      TEXT NOT NULL CHECK (provider IN ('google', 'facebook')),
    provider_sub  TEXT NOT NULL,
    email_at_link TEXT,
    linked_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, provider, provider_sub),
    UNIQUE (user_id, provider)
);

-- Pending authorisations: single use, short-lived.
CREATE TABLE oauth_states (
    state_hash    BYTEA PRIMARY KEY,
    tenant_id     TEXT NOT NULL,
    provider      TEXT NOT NULL,
    code_verifier TEXT NOT NULL,
    redirect_url  TEXT NOT NULL,
    visitor_id    UUID,                    -- the caller at start; the cart merge source
    expires_at    TIMESTAMPTZ NOT NULL
);

CREATE TABLE password_reset_tokens (
    token_hash BYTEA PRIMARY KEY,
    id         UUID NOT NULL UNIQUE,       -- request_id in the event
    tenant_id  TEXT NOT NULL,
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ
);
CREATE INDEX idx_password_reset_tokens_user ON password_reset_tokens (user_id);

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['refresh_tokens', 'revoked_access_tokens', 'oauth_identities',
                             'oauth_states', 'password_reset_tokens'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING (tenant_id = current_setting('app.tenant_id', true)
                   OR current_setting('app.tenant_id', true) = '_platform')
            WITH CHECK (tenant_id = current_setting('app.tenant_id', true))$p$, t);
    END LOOP;
END $$;
