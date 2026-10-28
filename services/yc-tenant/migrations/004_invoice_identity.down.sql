ALTER TABLE tenants
    DROP COLUMN IF EXISTS vat_number,
    DROP COLUMN IF EXISTS invoice_address;
