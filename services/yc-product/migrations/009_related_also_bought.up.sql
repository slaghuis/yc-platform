-- Editorial related products (docs/features/Related products and also-bought.md).
-- Links are directed: A -> B does not imply B -> A. The composite foreign keys
-- keep both ends in the row's own shop.
CREATE TABLE product_related (
    tenant_id          TEXT NOT NULL,
    product_id         UUID NOT NULL,
    related_product_id UUID NOT NULL,
    sort_order         INT NOT NULL DEFAULT 0,
    PRIMARY KEY (product_id, related_product_id),
    CHECK (product_id <> related_product_id),
    FOREIGN KEY (tenant_id, product_id) REFERENCES products (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, related_product_id) REFERENCES products (tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX idx_product_related_order ON product_related (tenant_id, product_id, sort_order);
CREATE INDEX idx_product_related_related ON product_related (related_product_id);

ALTER TABLE product_related ENABLE ROW LEVEL SECURITY;
ALTER TABLE product_related FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON product_related
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform')
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Also-bought: a projection of order.placed.v1, one row per ordered pair of
-- products and direction. No foreign keys: it is derived data, and reads
-- filter to active products.
CREATE TABLE product_also_bought (
    tenant_id        TEXT NOT NULL,
    product_id       UUID NOT NULL,
    other_product_id UUID NOT NULL,
    order_count      BIGINT NOT NULL DEFAULT 0,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, product_id, other_product_id),
    CHECK (product_id <> other_product_id)
);
CREATE INDEX idx_also_bought_rank ON product_also_bought (tenant_id, product_id, order_count DESC);

ALTER TABLE product_also_bought ENABLE ROW LEVEL SECURITY;
ALTER TABLE product_also_bought FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON product_also_bought
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform')
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
