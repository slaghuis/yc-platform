-- Stock and reservations (docs/services/yc-inventory.md, Database Schema).

-- A shop's inventory settings; shops without a row use the service
-- defaults.
CREATE TABLE tenant_inventory_config (
    tenant_id                       TEXT PRIMARY KEY,
    inventory_enabled               BOOLEAN NOT NULL DEFAULT TRUE,
    default_reservation_ttl_minutes INT NOT NULL CHECK (default_reservation_ttl_minutes > 0),
    min_reservation_ttl_minutes     INT NOT NULL CHECK (min_reservation_ttl_minutes > 0),
    max_reservation_ttl_minutes     INT NOT NULL CHECK (max_reservation_ttl_minutes > 0),
    created_at                      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at                      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (min_reservation_ttl_minutes <= default_reservation_ttl_minutes
           AND default_reservation_ttl_minutes <= max_reservation_ttl_minutes)
);

-- One row per tracked SKU. reserved may exceed on_hand only after an
-- oversell (a reservation committed after it expired), so available
-- (on_hand - reserved) can be negative.
CREATE TABLE stock_levels (
    tenant_id     TEXT NOT NULL,
    sku_id        UUID NOT NULL,
    on_hand       BIGINT NOT NULL DEFAULT 0 CHECK (on_hand >= 0),
    reserved      BIGINT NOT NULL DEFAULT 0 CHECK (reserved >= 0),
    low_threshold BIGINT NOT NULL DEFAULT 0 CHECK (low_threshold >= 0),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, sku_id)
);

-- Every change to a stock level, with the signed change to on_hand and to
-- reserved.
CREATE TABLE stock_movements (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      TEXT NOT NULL,
    sku_id         UUID NOT NULL,
    movement_type  TEXT NOT NULL CHECK (movement_type IN ('receive', 'adjust', 'reserve', 'release', 'commit', 'expire')),
    on_hand_delta  BIGINT NOT NULL,
    reserved_delta BIGINT NOT NULL,
    reason         TEXT,
    actor_id       TEXT,
    reference      TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_stock_movements_sku ON stock_movements (tenant_id, sku_id, created_at, id);

CREATE TABLE reservations (
    id              UUID PRIMARY KEY,
    tenant_id       TEXT NOT NULL,
    session_id      UUID NOT NULL,
    idempotency_key TEXT NOT NULL,
    status          TEXT NOT NULL CHECK (status IN ('active', 'committed', 'released', 'expired')),
    expires_at      TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    settled_at      TIMESTAMPTZ,
    UNIQUE (tenant_id, idempotency_key)
);
CREATE INDEX idx_reservations_active ON reservations (expires_at) WHERE status = 'active';

CREATE TABLE reservation_items (
    reservation_id UUID NOT NULL REFERENCES reservations (id) ON DELETE CASCADE,
    tenant_id      TEXT NOT NULL,
    sku_id         UUID NOT NULL,
    quantity       BIGINT NOT NULL CHECK (quantity > 0),
    PRIMARY KEY (reservation_id, sku_id)
);
CREATE INDEX idx_reservation_items_sku ON reservation_items (tenant_id, sku_id);

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['tenant_inventory_config', 'stock_levels', 'stock_movements', 'reservations', 'reservation_items'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING (tenant_id = current_setting('app.tenant_id', true)
                   OR current_setting('app.tenant_id', true) = '_platform')
            WITH CHECK (tenant_id = current_setting('app.tenant_id', true))$p$, t);
    END LOOP;
END $$;
