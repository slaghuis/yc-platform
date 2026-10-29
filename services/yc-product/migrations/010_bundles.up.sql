-- Flat product bundles (docs/features/Composed catalogue offers bundles and buy-X-get-Y.md, #34).
-- products.kind distinguishes ordinary catalogue items from virtual kits.
-- Composition is SKU-level, flat only: components are non-bundle physical SKUs.

ALTER TABLE products
    ADD COLUMN kind TEXT NOT NULL DEFAULT 'standard'
        CHECK (kind IN ('standard', 'bundle'));

-- Composite FKs need a matching unique key (products already have one).
ALTER TABLE skus ADD CONSTRAINT uq_skus_tenant_id UNIQUE (tenant_id, id);

CREATE TABLE bundle_sku_components (
    tenant_id         TEXT NOT NULL,
    bundle_sku_id     UUID NOT NULL,
    component_sku_id  UUID NOT NULL,
    quantity          INT NOT NULL CHECK (quantity >= 1),
    sort_order        INT NOT NULL DEFAULT 0,
    PRIMARY KEY (bundle_sku_id, component_sku_id),
    CHECK (bundle_sku_id <> component_sku_id),
    FOREIGN KEY (tenant_id, bundle_sku_id) REFERENCES skus (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, component_sku_id) REFERENCES skus (tenant_id, id)
);
CREATE INDEX idx_bundle_components_order ON bundle_sku_components (tenant_id, bundle_sku_id, sort_order);
CREATE INDEX idx_bundle_components_component ON bundle_sku_components (component_sku_id);

ALTER TABLE bundle_sku_components ENABLE ROW LEVEL SECURITY;
ALTER TABLE bundle_sku_components FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON bundle_sku_components
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform')
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
