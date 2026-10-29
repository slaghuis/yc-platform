DROP TABLE IF EXISTS bundle_sku_components;
ALTER TABLE skus DROP CONSTRAINT IF EXISTS uq_skus_tenant_id;
ALTER TABLE products DROP COLUMN IF EXISTS kind;
