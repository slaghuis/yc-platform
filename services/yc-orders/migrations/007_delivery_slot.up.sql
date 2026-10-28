-- Delivery window snapshot at order place (issue #30).
ALTER TABLE orders
    ADD COLUMN IF NOT EXISTS delivery_slot JSONB;
