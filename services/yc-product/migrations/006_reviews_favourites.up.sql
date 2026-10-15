-- Review and like stats per product, fed by review.* events (Phase 7).
-- version is yc-review's counter for the product's stats: an event with an
-- older version than the stored one is ignored.
CREATE TABLE product_review_stats (
    tenant_id    TEXT NOT NULL,
    product_id   UUID PRIMARY KEY,
    rating_count INT NOT NULL CHECK (rating_count >= 0),
    rating_sum   BIGINT NOT NULL CHECK (rating_sum >= 0),
    like_count   INT NOT NULL CHECK (like_count >= 0),
    version      BIGINT NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- The products each customer has ordered (order.placed.v1), for the
-- "My favourites" facet and filter. Holds user IDs: personal data.
CREATE TABLE customer_favourites (
    tenant_id         TEXT NOT NULL,
    user_id           UUID NOT NULL,
    product_id        UUID NOT NULL,
    order_count       INT NOT NULL,
    last_ordered_at   TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, user_id, product_id)
);

-- Ratings are per product, so the unused per-SKU columns go.
ALTER TABLE sku_search_projection
    DROP COLUMN avg_rating,
    DROP COLUMN rating_count,
    DROP COLUMN like_count;

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['product_review_stats', 'customer_favourites'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING (tenant_id = current_setting('app.tenant_id', true)
                   OR current_setting('app.tenant_id', true) = '_platform')
            WITH CHECK (tenant_id = current_setting('app.tenant_id', true))$p$, t);
    END LOOP;
END $$;
