DROP TABLE IF EXISTS order_refunds;
DROP TABLE IF EXISTS order_returns;
ALTER TABLE orders
    DROP COLUMN IF EXISTS shipment_id,
    DROP COLUMN IF EXISTS tracking_number,
    DROP COLUMN IF EXISTS carrier_name,
    DROP COLUMN IF EXISTS carrier_reference,
    DROP COLUMN IF EXISTS packaged_at,
    DROP COLUMN IF EXISTS shipped_at,
    DROP COLUMN IF EXISTS delivered_at;
ALTER TABLE orders DROP CONSTRAINT orders_status_check;
ALTER TABLE orders ADD CONSTRAINT orders_status_check
    CHECK (status IN ('pending_payment', 'abandoned', 'received', 'cancelled'));
