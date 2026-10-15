-- Zones, rates and shipments (docs/services/yc-shipping.md, Database Schema).

-- A shop-defined group of regions. match_rules is an array of
-- {country_code, fields}; zones are matched lowest priority first.
CREATE TABLE zones (
    id          UUID PRIMARY KEY,
    tenant_id   TEXT NOT NULL,
    name        TEXT NOT NULL,
    match_rules JSONB NOT NULL,
    priority    INT NOT NULL DEFAULT 100 CHECK (priority BETWEEN 0 AND 1000),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_zones_tenant ON zones (tenant_id, priority, name, id);

-- A shipping method in a zone. Amounts are in the store currency.
CREATE TABLE rates (
    id                 UUID PRIMARY KEY,
    tenant_id          TEXT NOT NULL,
    zone_id            UUID NOT NULL REFERENCES zones (id) ON DELETE CASCADE,
    method             TEXT NOT NULL,
    display_name       TEXT NOT NULL,
    pricing_type       TEXT NOT NULL CHECK (pricing_type IN ('flat', 'weight_tiered', 'price_tiered')),
    flat_amount_minor  BIGINT NOT NULL DEFAULT 0,
    weight_tiers       JSONB NOT NULL DEFAULT '[]',
    price_tiers        JSONB NOT NULL DEFAULT '[]',
    estimated_days_min INT,
    estimated_days_max INT,
    carrier_driver     TEXT NOT NULL,
    status             TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'inactive')),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (zone_id, method)
);
CREATE INDEX idx_rates_tenant ON rates (tenant_id, zone_id);

-- One parcel per order.
CREATE TABLE shipments (
    id                UUID PRIMARY KEY,
    tenant_id         TEXT NOT NULL,
    order_id          UUID NOT NULL,
    order_number      TEXT NOT NULL,
    rate_id           UUID,
    method            TEXT NOT NULL,
    carrier_driver    TEXT NOT NULL,
    tracking_number   TEXT NOT NULL,
    label_url         TEXT,
    status            TEXT NOT NULL CHECK (status IN ('label_created', 'in_transit', 'delivered', 'exception', 'cancelled')),
    delivery_address  JSONB NOT NULL,
    carrier_name      TEXT,
    carrier_reference TEXT,
    carrier_metadata  JSONB NOT NULL DEFAULT '{}',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, order_id),
    UNIQUE (tenant_id, tracking_number)
);
CREATE INDEX idx_shipments_list ON shipments (tenant_id, created_at DESC, id DESC);

CREATE TABLE shipment_events (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   TEXT NOT NULL,
    shipment_id UUID NOT NULL REFERENCES shipments (id) ON DELETE CASCADE,
    status      TEXT NOT NULL,
    message     TEXT,
    actor_id    TEXT,
    occurred_at TIMESTAMPTZ NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_shipment_events_shipment ON shipment_events (shipment_id, occurred_at, recorded_at);

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['zones', 'rates', 'shipments', 'shipment_events'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING (tenant_id = current_setting('app.tenant_id', true)
                   OR current_setting('app.tenant_id', true) = '_platform')
            WITH CHECK (tenant_id = current_setting('app.tenant_id', true))$p$, t);
    END LOOP;
END $$;
