-- Templates, messages and verification codes (docs/services/yc-message.md,
-- Database Schema). The platform's default templates are built in; this
-- table holds shops' own.
CREATE TABLE templates (
    tenant_id    TEXT NOT NULL,
    message_type TEXT NOT NULL,
    channel      TEXT NOT NULL CHECK (channel IN ('email', 'sms')),
    locale       TEXT NOT NULL,
    subject      TEXT NOT NULL DEFAULT '',
    body         TEXT NOT NULL,
    body_format  TEXT NOT NULL CHECK (body_format IN ('text', 'html')),
    version      INT NOT NULL DEFAULT 1,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, message_type, channel, locale)
);

-- One rendered message to one address. reference makes a send idempotent.
CREATE TABLE messages (
    id                  UUID PRIMARY KEY,
    tenant_id           TEXT NOT NULL,
    message_type        TEXT NOT NULL,
    recipient_user_id   TEXT,
    recipient_address   TEXT NOT NULL,
    channel             TEXT NOT NULL,
    locale              TEXT NOT NULL,
    subject             TEXT NOT NULL DEFAULT '',
    body_rendered       TEXT NOT NULL,
    body_format         TEXT NOT NULL,
    status              TEXT NOT NULL CHECK (status IN ('queued', 'sent', 'delivered', 'bounced', 'failed')),
    driver              TEXT NOT NULL,
    provider_message_id TEXT,
    attempts            INT NOT NULL DEFAULT 0,
    last_error          TEXT,
    next_attempt_at     TIMESTAMPTZ,
    context             JSONB NOT NULL DEFAULT '{}',
    reference           TEXT NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    sent_at             TIMESTAMPTZ,
    delivered_at        TIMESTAMPTZ,
    UNIQUE (tenant_id, reference)
);
CREATE INDEX idx_messages_due ON messages (next_attempt_at) WHERE status = 'queued';
CREATE INDEX idx_messages_list ON messages (tenant_id, created_at DESC, id DESC);
CREATE INDEX idx_messages_recipient ON messages (tenant_id, recipient_user_id, created_at DESC);
CREATE INDEX idx_messages_provider ON messages (tenant_id, provider_message_id);

-- A code proving a user owns a channel address. Only the hash is stored.
CREATE TABLE verifications (
    id           UUID PRIMARY KEY,
    tenant_id    TEXT NOT NULL,
    user_id      TEXT NOT NULL,
    channel      TEXT NOT NULL,
    address      TEXT NOT NULL,
    code_hash    TEXT NOT NULL,
    attempts     INT NOT NULL DEFAULT 0,
    expires_at   TIMESTAMPTZ NOT NULL,
    confirmed_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_verifications_user ON verifications (tenant_id, user_id, channel, created_at DESC);

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['templates', 'messages', 'verifications'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING (tenant_id = current_setting('app.tenant_id', true)
                   OR current_setting('app.tenant_id', true) = '_platform')
            WITH CHECK (tenant_id = current_setting('app.tenant_id', true))$p$, t);
    END LOOP;
END $$;
