ALTER TABLE orders
    DROP COLUMN IF EXISTS billing_address,
    DROP COLUMN IF EXISTS seller_invoice;
