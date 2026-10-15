ALTER TABLE transactions DROP COLUMN kind;

ALTER TABLE tenant_gateway_config
    DROP COLUMN currencies,
    DROP COLUMN priority;
