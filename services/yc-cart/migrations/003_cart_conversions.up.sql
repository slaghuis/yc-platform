-- Checkout sessions already removed from carts (checkout.completed.v1).
-- Inserted in the conversion's transaction so a redelivered event never
-- subtracts twice.
CREATE TABLE cart_conversions (
    tenant_id    TEXT NOT NULL,
    session_id   UUID NOT NULL,
    order_id     UUID NOT NULL,
    converted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, session_id)
);

ALTER TABLE cart_conversions ENABLE ROW LEVEL SECURITY;
ALTER TABLE cart_conversions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON cart_conversions
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform')
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
