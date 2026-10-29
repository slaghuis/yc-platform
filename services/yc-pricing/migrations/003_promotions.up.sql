-- Sale overlays and buy-X-get-Y campaigns (issue #31 / composed offers FR).

ALTER TABLE sku_tax_classes ADD COLUMN IF NOT EXISTS product_id UUID;

CREATE TABLE sale_prices (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     TEXT NOT NULL,
    sku_id        UUID NOT NULL,
    amount_minor  BIGINT NOT NULL CHECK (amount_minor >= 0),
    currency      TEXT NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    tax_inclusive BOOLEAN NOT NULL,
    name          TEXT NOT NULL DEFAULT '',
    starts_at     TIMESTAMPTZ NOT NULL,
    ends_at       TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (ends_at IS NULL OR ends_at > starts_at)
);
CREATE UNIQUE INDEX uq_sale_prices_sku ON sale_prices (tenant_id, sku_id);
CREATE INDEX idx_sale_prices_window ON sale_prices (tenant_id, starts_at, ends_at);

CREATE TABLE bxgy_campaigns (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             TEXT NOT NULL,
    name                  TEXT NOT NULL CHECK (name <> ''),
    qualifying_product_id UUID,
    qualifying_sku_id     UUID,
    target_product_id     UUID,
    target_sku_id         UUID,
    discount_type         TEXT NOT NULL CHECK (discount_type IN ('percent', 'fixed')),
    discount_value        BIGINT NOT NULL CHECK (discount_value > 0),
    starts_at             TIMESTAMPTZ NOT NULL,
    ends_at               TIMESTAMPTZ,
    enabled               BOOLEAN NOT NULL DEFAULT TRUE,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (ends_at IS NULL OR ends_at > starts_at),
    CHECK (
        (qualifying_product_id IS NOT NULL AND qualifying_sku_id IS NULL)
        OR (qualifying_product_id IS NULL AND qualifying_sku_id IS NOT NULL)
    ),
    CHECK (
        (target_product_id IS NOT NULL AND target_sku_id IS NULL)
        OR (target_product_id IS NULL AND target_sku_id IS NOT NULL)
    ),
    CHECK (
        discount_type <> 'percent' OR discount_value <= 10000
    )
);
CREATE INDEX idx_bxgy_active ON bxgy_campaigns (tenant_id, enabled, starts_at, ends_at);

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['sale_prices', 'bxgy_campaigns'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING (tenant_id = current_setting('app.tenant_id', true)
                   OR current_setting('app.tenant_id', true) = '_platform')
            WITH CHECK (tenant_id = current_setting('app.tenant_id', true))$p$, t);
    END LOOP;
END $$;
