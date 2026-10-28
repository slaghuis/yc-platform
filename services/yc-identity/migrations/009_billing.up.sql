-- Customer billing address for SARS-oriented invoices (issue #29).
CREATE TABLE user_billing (
    user_id            UUID PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    tenant_id          TEXT NOT NULL,
    company_name       TEXT NOT NULL DEFAULT '',
    vat_number         TEXT NOT NULL DEFAULT '',
    street_and_number  TEXT NOT NULL,
    suburb             TEXT NOT NULL DEFAULT '',
    town_city          TEXT NOT NULL,
    postal_code        TEXT NOT NULL,
    country_code       TEXT NOT NULL DEFAULT 'ZA',
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX user_billing_tenant ON user_billing (tenant_id);

DO $$
BEGIN
    EXECUTE 'ALTER TABLE user_billing ENABLE ROW LEVEL SECURITY';
    EXECUTE 'ALTER TABLE user_billing FORCE ROW LEVEL SECURITY';
    EXECUTE $p$CREATE POLICY tenant_isolation ON user_billing
        USING (tenant_id = current_setting('app.tenant_id', true)
               OR current_setting('app.tenant_id', true) = '_platform')
        WITH CHECK (tenant_id = current_setting('app.tenant_id', true))$p$;
END $$;
