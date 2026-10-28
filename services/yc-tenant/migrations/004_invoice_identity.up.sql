-- Shop VAT and seller invoice address for SARS-oriented invoices (issue #29).
ALTER TABLE tenants
    ADD COLUMN IF NOT EXISTS vat_number TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS invoice_address JSONB;
