-- Carts (docs/services/yc-cart.md, Database Schema). Cart types come from
-- configuration for now; tenant_cart_config arrives with the admin API.

CREATE TABLE carts (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        TEXT NOT NULL,
    user_id          UUID NOT NULL,          -- visitor or customer; registration keeps the ID
    cart_type        TEXT NOT NULL CHECK (cart_type ~ '^[a-z][a-z0-9_]{0,31}$'),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, user_id, cart_type)
);

-- unit_price is the gross unit price in the store currency when the line
-- was first added: a snapshot for change detection, not authoritative.
CREATE TABLE cart_items (
    tenant_id           TEXT NOT NULL,
    cart_id             UUID NOT NULL REFERENCES carts (id) ON DELETE CASCADE,
    sku_id              UUID NOT NULL,
    quantity            INT NOT NULL CHECK (quantity > 0),
    unit_price_minor    BIGINT NOT NULL CHECK (unit_price_minor >= 0),
    unit_price_currency TEXT NOT NULL CHECK (unit_price_currency ~ '^[A-Z]{3}$'),
    added_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (cart_id, sku_id)
);
CREATE INDEX idx_cart_items_sku ON cart_items (tenant_id, sku_id);

-- Customers' display currencies from preference.currency.changed.v1.
CREATE TABLE display_currencies (
    tenant_id        TEXT NOT NULL,
    user_id          UUID NOT NULL,
    display_currency TEXT NOT NULL CHECK (display_currency ~ '^[A-Z]{3}$'),
    changed_at       TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, user_id)
);

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['carts', 'cart_items', 'display_currencies'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING (tenant_id = current_setting('app.tenant_id', true)
                   OR current_setting('app.tenant_id', true) = '_platform')
            WITH CHECK (tenant_id = current_setting('app.tenant_id', true))$p$, t);
    END LOOP;
END $$;
