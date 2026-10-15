-- Pricing (docs/services/yc-pricing.md, Database Schema; ADR-014, ADR-016).

CREATE TABLE tenant_pricing_config (
    tenant_id            TEXT PRIMARY KEY,
    store_currency       TEXT NOT NULL CHECK (store_currency ~ '^[A-Z]{3}$'),
    tax_inclusive        BOOLEAN NOT NULL,
    default_jurisdiction TEXT NOT NULL CHECK (default_jurisdiction ~ '^[A-Z]{2}(-[A-Z0-9]{1,3})?$'),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Price history: the current price has valid_to NULL; setting a new one
-- closes it.
CREATE TABLE base_prices (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     TEXT NOT NULL,
    sku_id        UUID NOT NULL,
    amount_minor  BIGINT NOT NULL CHECK (amount_minor >= 0),
    currency      TEXT NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    tax_inclusive BOOLEAN NOT NULL,
    valid_from    TIMESTAMPTZ NOT NULL,
    valid_to      TIMESTAMPTZ,
    set_by        TEXT NOT NULL DEFAULT '',
    CHECK (valid_to IS NULL OR valid_to >= valid_from)
);
CREATE UNIQUE INDEX uq_base_prices_current ON base_prices (tenant_id, sku_id) WHERE valid_to IS NULL;
CREATE INDEX idx_base_prices_history ON base_prices (tenant_id, sku_id, valid_from DESC);

CREATE TABLE tax_classes (
    tenant_id    TEXT NOT NULL,
    class_id     TEXT NOT NULL CHECK (class_id ~ '^[a-z0-9]+(-[a-z0-9]+)*$'),
    display_name TEXT NOT NULL CHECK (display_name <> ''),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, class_id)
);

CREATE TABLE tax_rules (
    tenant_id    TEXT NOT NULL,
    class_id     TEXT NOT NULL,
    jurisdiction TEXT NOT NULL CHECK (jurisdiction ~ '^[A-Z]{2}(-[A-Z0-9]{1,3})?$'),
    rule_name    TEXT NOT NULL CHECK (rule_name <> ''),
    rate         NUMERIC(12,8) NOT NULL CHECK (rate >= 0),
    base         TEXT NOT NULL,
    sort_order   INT NOT NULL,
    PRIMARY KEY (tenant_id, class_id, jurisdiction, rule_name),
    FOREIGN KEY (tenant_id, class_id) REFERENCES tax_classes (tenant_id, class_id) ON DELETE CASCADE
);

-- Each SKU's tax class, from product.sku.* events. event_at orders
-- out-of-order deliveries; deleted SKUs stay deleted.
CREATE TABLE sku_tax_classes (
    tenant_id  TEXT NOT NULL,
    sku_id     UUID NOT NULL,
    tax_class  TEXT NOT NULL,
    status     TEXT NOT NULL CHECK (status IN ('active', 'inactive', 'deleted')),
    event_at   TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, sku_id)
);
CREATE INDEX idx_sku_tax_classes_class ON sku_tax_classes (tenant_id, tax_class) WHERE status <> 'deleted';

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['tenant_pricing_config', 'base_prices', 'tax_classes', 'tax_rules',
                             'sku_tax_classes'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING (tenant_id = current_setting('app.tenant_id', true)
                   OR current_setting('app.tenant_id', true) = '_platform')
            WITH CHECK (tenant_id = current_setting('app.tenant_id', true))$p$, t);
    END LOOP;
END $$;

-- Exchange rates are platform data shared by every shop: no tenant, no RLS.
CREATE TABLE exchange_rates (
    base_currency  TEXT NOT NULL CHECK (base_currency ~ '^[A-Z]{3}$'),
    quote_currency TEXT NOT NULL CHECK (quote_currency ~ '^[A-Z]{3}$'),
    rate           NUMERIC(24,12) NOT NULL CHECK (rate > 0),
    provider       TEXT NOT NULL,
    fetched_at     TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (base_currency, quote_currency)
);

-- One row per base currency: when its rates were last refreshed, and when
-- staleness was last reported.
CREATE TABLE exchange_rate_fetches (
    base_currency     TEXT PRIMARY KEY CHECK (base_currency ~ '^[A-Z]{3}$'),
    provider          TEXT NOT NULL,
    last_success_at   TIMESTAMPTZ,
    last_attempt_at   TIMESTAMPTZ NOT NULL,
    last_error        TEXT NOT NULL DEFAULT '',
    stale_reported_at TIMESTAMPTZ
);
