-- Customer delivery address book (issue #32). Soft-delete only; never hard-delete
-- except CASCADE on user deletion (GDPR).
CREATE TABLE customer_delivery_addresses (
    id              UUID PRIMARY KEY,
    tenant_id       TEXT NOT NULL,
    user_id         UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    label           TEXT NOT NULL DEFAULT '',
    address         JSONB NOT NULL,
    is_default      BOOLEAN NOT NULL DEFAULT FALSE,
    deleted_at      TIMESTAMPTZ,
    replaced_by_id  UUID REFERENCES customer_delivery_addresses (id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX customer_delivery_addresses_user_active
    ON customer_delivery_addresses (tenant_id, user_id, created_at)
    WHERE deleted_at IS NULL;

CREATE UNIQUE INDEX customer_delivery_addresses_one_default
    ON customer_delivery_addresses (tenant_id, user_id)
    WHERE deleted_at IS NULL AND is_default;

DO $$
BEGIN
    EXECUTE 'ALTER TABLE customer_delivery_addresses ENABLE ROW LEVEL SECURITY';
    EXECUTE 'ALTER TABLE customer_delivery_addresses FORCE ROW LEVEL SECURITY';
    EXECUTE $p$CREATE POLICY tenant_isolation ON customer_delivery_addresses
        USING (tenant_id = current_setting('app.tenant_id', true)
               OR current_setting('app.tenant_id', true) = '_platform')
        WITH CHECK (tenant_id = current_setting('app.tenant_id', true))$p$;
END $$;
