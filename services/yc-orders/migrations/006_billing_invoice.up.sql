-- Buyer billing and seller invoice snapshots for SARS-oriented invoices (issue #29).
ALTER TABLE orders
    ADD COLUMN IF NOT EXISTS billing_address JSONB,
    ADD COLUMN IF NOT EXISTS seller_invoice JSONB;
