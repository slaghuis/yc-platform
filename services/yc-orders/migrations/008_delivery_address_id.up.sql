-- Book entry id snapshotted at order place (issue #32).
ALTER TABLE orders
    ADD COLUMN IF NOT EXISTS delivery_address_id UUID;

CREATE INDEX IF NOT EXISTS orders_delivery_address_id
    ON orders (tenant_id, delivery_address_id)
    WHERE delivery_address_id IS NOT NULL;
