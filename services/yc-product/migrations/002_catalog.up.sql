-- Catalog (docs/services/yc-product.md, Database Schema). Every table
-- carries tenant_id so one RLS policy shape covers all of them.

CREATE TABLE suppliers (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   TEXT NOT NULL,
    name        TEXT NOT NULL CHECK (name <> ''),
    contact     JSONB NOT NULL DEFAULT '{}',
    status      TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'inactive')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, id)
);
CREATE INDEX idx_suppliers_tenant_created ON suppliers (tenant_id, created_at DESC, id DESC);

CREATE TABLE products (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   TEXT NOT NULL,
    supplier_id UUID,
    name        TEXT NOT NULL CHECK (name <> ''),
    description TEXT NOT NULL DEFAULT '',
    is_digital  BOOLEAN NOT NULL DEFAULT FALSE,
    status      TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'inactive', 'deleted')),
    attributes  JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, id),
    -- A product's supplier belongs to the same tenant.
    FOREIGN KEY (tenant_id, supplier_id) REFERENCES suppliers (tenant_id, id)
);
CREATE INDEX idx_products_tenant_created ON products (tenant_id, created_at DESC, id DESC);
CREATE INDEX idx_products_supplier ON products (supplier_id);

CREATE TABLE product_variants (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   TEXT NOT NULL,
    product_id  UUID NOT NULL,
    axis_name   TEXT NOT NULL CHECK (axis_name ~ '^[a-z][a-z0-9_]*$'),
    values      JSONB NOT NULL CHECK (jsonb_typeof(values) = 'array'),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (product_id, axis_name),
    FOREIGN KEY (tenant_id, product_id) REFERENCES products (tenant_id, id) ON DELETE CASCADE
);

CREATE TABLE skus (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         TEXT NOT NULL,
    product_id        UUID NOT NULL,
    sku_code          TEXT NOT NULL CHECK (sku_code <> ''),
    variant_values    JSONB NOT NULL DEFAULT '{}',
    inventory_tracked BOOLEAN NOT NULL DEFAULT TRUE,
    tax_class         TEXT NOT NULL,
    weight_grams      INT CHECK (weight_grams >= 0),
    status            TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'inactive', 'deleted')),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (tenant_id, product_id) REFERENCES products (tenant_id, id) ON DELETE CASCADE
);
-- Deleted SKUs release their code.
CREATE UNIQUE INDEX uq_skus_tenant_code ON skus (tenant_id, sku_code) WHERE status <> 'deleted';
CREATE INDEX idx_skus_product ON skus (product_id);

CREATE TABLE categories (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   TEXT NOT NULL,
    parent_id   UUID,
    name        TEXT NOT NULL CHECK (name <> ''),
    slug        TEXT NOT NULL CHECK (slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$'),
    sort_order  INT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, slug),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, parent_id) REFERENCES categories (tenant_id, id)
);

CREATE TABLE product_categories (
    tenant_id   TEXT NOT NULL,
    product_id  UUID NOT NULL,
    category_id UUID NOT NULL,
    PRIMARY KEY (product_id, category_id),
    FOREIGN KEY (tenant_id, product_id) REFERENCES products (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, category_id) REFERENCES categories (tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX idx_product_categories_category ON product_categories (category_id);

CREATE TABLE product_images (
    tenant_id   TEXT NOT NULL,
    product_id  UUID NOT NULL,
    image_id    UUID NOT NULL,      -- yc-image
    sort_order  INT NOT NULL DEFAULT 0,
    is_primary  BOOLEAN NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (product_id, image_id),
    FOREIGN KEY (tenant_id, product_id) REFERENCES products (tenant_id, id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX uq_product_images_primary ON product_images (product_id) WHERE is_primary;

-- Search projection, one row per SKU, written with the SKU and product in
-- the same transaction. Event-driven columns (price, stock, popularity,
-- rating) arrive in Phase 3.
CREATE TABLE sku_search_projection (
    sku_id           UUID PRIMARY KEY REFERENCES skus (id) ON DELETE CASCADE,
    tenant_id        TEXT NOT NULL,
    product_id       UUID NOT NULL,
    supplier_id      UUID,
    category_ids     UUID[] NOT NULL DEFAULT '{}',
    name             TEXT NOT NULL,
    description      TEXT NOT NULL DEFAULT '',
    sku_code         TEXT NOT NULL,
    tsv              TSVECTOR NOT NULL,
    variant_values   JSONB NOT NULL DEFAULT '{}',
    base_price       BIGINT,
    in_stock         BOOLEAN,
    popularity_score BIGINT NOT NULL DEFAULT 0,
    avg_rating       NUMERIC(3,2),
    rating_count     INT NOT NULL DEFAULT 0,
    like_count       INT NOT NULL DEFAULT 0,
    status           TEXT NOT NULL,   -- active only when both product and SKU are
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_sku_search_tsv ON sku_search_projection USING GIN (tsv);
CREATE INDEX idx_sku_search_categories ON sku_search_projection USING GIN (category_ids);
CREATE INDEX idx_sku_search_tenant_product ON sku_search_projection (tenant_id, product_id);

-- The service user owns the tables, so FORCE is needed for RLS to apply.
DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['suppliers', 'products', 'product_variants', 'skus', 'categories',
                             'product_categories', 'product_images', 'sku_search_projection'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING (tenant_id = current_setting('app.tenant_id', true)
                   OR current_setting('app.tenant_id', true) = '_platform')
            WITH CHECK (tenant_id = current_setting('app.tenant_id', true))$p$, t);
    END LOOP;
END $$;
