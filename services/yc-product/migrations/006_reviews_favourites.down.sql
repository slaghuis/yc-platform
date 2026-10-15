ALTER TABLE sku_search_projection
    ADD COLUMN avg_rating   NUMERIC(3,2),
    ADD COLUMN rating_count INT NOT NULL DEFAULT 0,
    ADD COLUMN like_count   INT NOT NULL DEFAULT 0;
DROP TABLE IF EXISTS customer_favourites;
DROP TABLE IF EXISTS product_review_stats;
