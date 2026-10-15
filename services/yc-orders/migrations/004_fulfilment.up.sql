-- Fulfilment (Phase 6, ADR-022): an admin packages a received order, which
-- books its shipment in yc-shipping; shipping events move it on to
-- shipped, delivered or delivery_exception. Customers return delivered
-- items within 30 days; an accepted return is refunded at once.
ALTER TABLE orders DROP CONSTRAINT orders_status_check;
ALTER TABLE orders ADD CONSTRAINT orders_status_check CHECK (status IN ('pending_payment', 'abandoned', 'received',
    'cancelled', 'packaged', 'shipped', 'delivered', 'delivery_exception', 'returned'));
ALTER TABLE orders
    ADD COLUMN shipment_id       UUID,
    ADD COLUMN tracking_number   TEXT,
    ADD COLUMN carrier_name      TEXT,
    ADD COLUMN carrier_reference TEXT,
    ADD COLUMN packaged_at       TIMESTAMPTZ,
    ADD COLUMN shipped_at        TIMESTAMPTZ,
    ADD COLUMN delivered_at      TIMESTAMPTZ;

CREATE TABLE order_returns (
    id               UUID PRIMARY KEY,
    tenant_id        TEXT NOT NULL,
    order_id         UUID NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    status           TEXT NOT NULL CHECK (status IN ('requested', 'accepted', 'rejected')),
    items            JSONB NOT NULL,
    reason           TEXT,
    refund_shipping  BOOLEAN NOT NULL DEFAULT false,
    refund_minor     BIGINT NOT NULL DEFAULT 0,
    decision_reason  TEXT,
    requested_by     TEXT NOT NULL,
    decided_by       TEXT,
    requested_at     TIMESTAMPTZ NOT NULL,
    decided_at       TIMESTAMPTZ
);
-- An order has at most one return awaiting a decision.
CREATE UNIQUE INDEX idx_order_returns_open ON order_returns (tenant_id, order_id) WHERE status = 'requested';
CREATE INDEX idx_order_returns_order ON order_returns (tenant_id, order_id, requested_at);
CREATE INDEX idx_order_returns_list ON order_returns (tenant_id, status, requested_at DESC, id DESC);

-- Each refund made on an order. payment_key is the Idempotency-Key sent to
-- yc-payment, so a retried refund is recorded once.
CREATE TABLE order_refunds (
    id           UUID PRIMARY KEY,
    tenant_id    TEXT NOT NULL,
    order_id     UUID NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    return_id    UUID REFERENCES order_returns (id) ON DELETE SET NULL,
    amount_minor BIGINT NOT NULL CHECK (amount_minor > 0),
    status       TEXT NOT NULL CHECK (status IN ('pending', 'completed')),
    reason       TEXT NOT NULL,
    payment_key  TEXT NOT NULL,
    actor_id     TEXT,
    created_at   TIMESTAMPTZ NOT NULL,
    UNIQUE (tenant_id, payment_key)
);
CREATE INDEX idx_order_refunds_order ON order_refunds (tenant_id, order_id, created_at);

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['order_returns', 'order_refunds'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING (tenant_id = current_setting('app.tenant_id', true)
                   OR current_setting('app.tenant_id', true) = '_platform')
            WITH CHECK (tenant_id = current_setting('app.tenant_id', true))$p$, t);
    END LOOP;
END $$;
