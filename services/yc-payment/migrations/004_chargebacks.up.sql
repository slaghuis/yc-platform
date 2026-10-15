-- Phase 8: chargebacks (disputes) the gateway reports. The response to a
-- dispute is made in the gateway's dashboard; its outcome is not tracked.
CREATE TABLE chargebacks (
    id                UUID PRIMARY KEY,
    transaction_id    UUID NOT NULL REFERENCES transactions (id),
    tenant_id         TEXT NOT NULL,
    order_id          UUID,
    gateway           TEXT NOT NULL,
    gateway_reference TEXT NOT NULL,
    amount_minor      BIGINT NOT NULL CHECK (amount_minor > 0),
    currency          TEXT NOT NULL,
    reason            TEXT,
    received_at       TIMESTAMPTZ NOT NULL,
    UNIQUE (gateway, gateway_reference)
);
CREATE INDEX idx_chargebacks_transaction ON chargebacks (transaction_id);

ALTER TABLE chargebacks ENABLE ROW LEVEL SECURITY;
ALTER TABLE chargebacks FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON chargebacks
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform')
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
