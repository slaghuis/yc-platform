DROP TABLE IF EXISTS deleted_customers;
ALTER TABLE orders DROP COLUMN IF EXISTS chargeback_id;
-- Disputed orders stay as they are; the old constraint only checks new rows.
ALTER TABLE orders DROP CONSTRAINT orders_status_check;
ALTER TABLE orders ADD CONSTRAINT orders_status_check CHECK (status IN ('pending_payment', 'abandoned', 'received',
    'cancelled', 'packaged', 'shipped', 'delivered', 'delivery_exception', 'returned')) NOT VALID;
