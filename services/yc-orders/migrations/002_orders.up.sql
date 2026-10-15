-- An order is a snapshot: names, prices and taxes as checkout priced them,
-- never live catalogue data. yc-checkout creates it pending_payment; the
-- order number is given on confirmation, so abandoned checkouts leave no
-- gaps in a shop's numbering.
CREATE TABLE orders (
    id                     UUID PRIMARY KEY,
    tenant_id              TEXT NOT NULL,
    order_number           TEXT,
    checkout_session_id    UUID NOT NULL,
    user_id                TEXT NOT NULL,
    user_type              TEXT NOT NULL CHECK (user_type IN ('customer', 'visitor')),
    guest_contact          JSONB,
    source_email           TEXT,
    delivery_address       JSONB,
    requires_shipping      BOOLEAN NOT NULL,
    status                 TEXT NOT NULL CHECK (status IN ('pending_payment', 'abandoned', 'received', 'cancelled')),
    items                  JSONB NOT NULL,
    pricing_snapshot       JSONB NOT NULL,
    shipping_snapshot      JSONB,
    currency               TEXT NOT NULL,
    items_total_minor      BIGINT NOT NULL,
    shipping_minor         BIGINT NOT NULL,
    tax_total_minor        BIGINT NOT NULL,
    total_minor            BIGINT NOT NULL CHECK (total_minor >= 0),
    payment_transaction_id UUID,
    payment_gateway        TEXT,
    refunded_minor         BIGINT NOT NULL DEFAULT 0,
    created_at             TIMESTAMPTZ NOT NULL,
    updated_at             TIMESTAMPTZ NOT NULL,
    placed_at              TIMESTAMPTZ,
    UNIQUE (tenant_id, order_number),
    UNIQUE (tenant_id, checkout_session_id)
);
CREATE INDEX idx_orders_user ON orders (tenant_id, user_id, created_at DESC, id DESC);
CREATE INDEX idx_orders_list ON orders (tenant_id, created_at DESC, id DESC);
CREATE INDEX idx_orders_source_email ON orders (tenant_id, source_email) WHERE user_type = 'visitor';

CREATE TABLE order_status_history (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id    UUID NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    tenant_id   TEXT NOT NULL,
    from_status TEXT,
    to_status   TEXT NOT NULL,
    actor_id    TEXT,
    reason      TEXT,
    created_at  TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_order_status_history_order ON order_status_history (order_id, created_at);

-- Each shop's last order number.
CREATE TABLE order_counters (
    tenant_id   TEXT PRIMARY KEY,
    last_number BIGINT NOT NULL
);

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['orders', 'order_status_history', 'order_counters'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING (tenant_id = current_setting('app.tenant_id', true)
                   OR current_setting('app.tenant_id', true) = '_platform')
            WITH CHECK (tenant_id = current_setting('app.tenant_id', true))$p$, t);
    END LOOP;
END $$;
