-- Phase 8. GDPR (ADR-005): a deleted customer's orders keep their amounts
-- but their user ID becomes the pseudonym and their address and contact
-- are cleared. deleted_customers remembers each deletion, so an order
-- confirmed afterwards is pseudonymised too, and a repeated deletion
-- event changes nothing.
--
-- Chargebacks (ADR-025): a disputed payment moves its order to disputed.
ALTER TABLE orders DROP CONSTRAINT orders_status_check;
ALTER TABLE orders ADD CONSTRAINT orders_status_check CHECK (status IN ('pending_payment', 'abandoned', 'received',
    'cancelled', 'packaged', 'shipped', 'delivered', 'delivery_exception', 'returned', 'disputed'));
ALTER TABLE orders ADD COLUMN chargeback_id UUID;

CREATE TABLE deleted_customers (
    tenant_id  TEXT NOT NULL,
    user_id    TEXT NOT NULL,
    pseudonym  TEXT NOT NULL,
    deleted_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, user_id)
);

ALTER TABLE deleted_customers ENABLE ROW LEVEL SECURITY;
ALTER TABLE deleted_customers FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON deleted_customers
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform')
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
