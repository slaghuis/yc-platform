-- Event-fed search signals (Phase 3). base_price_at orders
-- pricing.base_price.* events so a late, older one cannot overwrite a newer
-- price.
ALTER TABLE sku_search_projection ADD COLUMN base_price_at TIMESTAMPTZ;
