-- Preferences (docs/services/yc-preference.md, Database Schema).

-- A shop's defaults for new customers; the store currency when provisioned.
CREATE TABLE preference_defaults (
    tenant_id        TEXT PRIMARY KEY,
    display_currency TEXT NOT NULL CHECK (display_currency ~ '^[A-Z]{3}$'),
    locale           TEXT NOT NULL CHECK (locale <> ''),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE preferences (
    tenant_id        TEXT NOT NULL,
    user_id          UUID NOT NULL,
    display_currency TEXT NOT NULL CHECK (display_currency ~ '^[A-Z]{3}$'),
    locale           TEXT NOT NULL CHECK (locale <> ''),
    primary_channel  TEXT CHECK (primary_channel IN ('email', 'sms', 'whatsapp', 'telegram')),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, user_id)
);

CREATE TABLE messaging_channels (
    tenant_id   TEXT NOT NULL,
    user_id     UUID NOT NULL,
    channel     TEXT NOT NULL CHECK (channel IN ('email', 'sms', 'whatsapp', 'telegram')),
    address     TEXT NOT NULL CHECK (address <> ''),
    verified    BOOLEAN NOT NULL DEFAULT FALSE,
    verified_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, user_id, channel),
    FOREIGN KEY (tenant_id, user_id) REFERENCES preferences (tenant_id, user_id) ON DELETE CASCADE
);

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['preference_defaults', 'preferences', 'messaging_channels'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING (tenant_id = current_setting('app.tenant_id', true)
                   OR current_setting('app.tenant_id', true) = '_platform')
            WITH CHECK (tenant_id = current_setting('app.tenant_id', true))$p$, t);
    END LOOP;
END $$;
