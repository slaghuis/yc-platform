-- in_stock comes from inventory.stock.depleted/restored.v1; in_stock_at
-- orders them. NULL in_stock (untracked SKU, or a shop without inventory)
-- counts as in stock.
ALTER TABLE sku_search_projection ADD COLUMN in_stock_at TIMESTAMPTZ;
