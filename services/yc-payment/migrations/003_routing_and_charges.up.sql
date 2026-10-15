-- Phase 5: per-currency gateway routing (ADR-013) and combined charges
-- (Variant B).
ALTER TABLE tenant_gateway_config
    ADD COLUMN priority   INT NOT NULL DEFAULT 100 CHECK (priority BETWEEN 0 AND 1000), -- lowest first
    ADD COLUMN currencies TEXT[] NOT NULL DEFAULT '{}';                                -- empty: all the driver takes

ALTER TABLE transactions
    ADD COLUMN kind TEXT NOT NULL DEFAULT 'authorize' CHECK (kind IN ('authorize', 'charge'));
