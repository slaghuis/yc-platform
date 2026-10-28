DROP INDEX IF EXISTS orders_delivery_address_id;
ALTER TABLE orders DROP COLUMN IF EXISTS delivery_address_id;
