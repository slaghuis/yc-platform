-- A shop's credentials for one gateway. credentials_enc is the AES-256-GCM
-- sealed JSON {api_key, webhook_secret}, bound to (tenant_id, gateway).
CREATE TABLE tenant_gateway_config (
    tenant_id          TEXT NOT NULL,
    gateway            TEXT NOT NULL,
    enabled            BOOLEAN NOT NULL DEFAULT TRUE,
    credentials_enc    BYTEA NOT NULL,
    api_key_hint       TEXT NOT NULL,            -- last characters, for display
    webhook_secret_set BOOLEAN NOT NULL,
    public_key         TEXT,                     -- for the storefront SDK
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, gateway)
);

-- One payment. The row is written before the gateway is called, so
-- gateway_reference is NULL until the gateway answers.
CREATE TABLE transactions (
    id                    UUID PRIMARY KEY,
    tenant_id             TEXT NOT NULL,
    session_id            TEXT NOT NULL,
    order_id              UUID,
    gateway               TEXT NOT NULL,
    gateway_reference     TEXT,
    status                TEXT NOT NULL CHECK (status IN
        ('created', 'pending', 'authorized', 'captured', 'voided', 'refunded', 'failed')),
    amount_minor          BIGINT NOT NULL CHECK (amount_minor > 0),
    currency              TEXT NOT NULL,
    captured_amount_minor BIGINT NOT NULL DEFAULT 0,
    refunded_amount_minor BIGINT NOT NULL DEFAULT 0,
    idempotency_key       TEXT NOT NULL,
    next_action           JSONB,
    failure_reason        TEXT,
    created_at            TIMESTAMPTZ NOT NULL,
    updated_at            TIMESTAMPTZ NOT NULL,
    UNIQUE (tenant_id, idempotency_key),
    UNIQUE (gateway, gateway_reference),
    CHECK (refunded_amount_minor <= captured_amount_minor)
);
CREATE INDEX idx_transactions_list ON transactions (tenant_id, created_at DESC, id DESC);
CREATE INDEX idx_transactions_session ON transactions (tenant_id, session_id);
CREATE INDEX idx_transactions_order ON transactions (tenant_id, order_id);

CREATE TABLE refunds (
    id                UUID PRIMARY KEY,
    transaction_id    UUID NOT NULL REFERENCES transactions (id),
    tenant_id         TEXT NOT NULL,
    order_id          UUID,
    amount_minor      BIGINT NOT NULL CHECK (amount_minor > 0),
    reason            TEXT,
    gateway_reference TEXT,
    status            TEXT NOT NULL CHECK (status IN ('pending', 'completed', 'failed')),
    failure_reason    TEXT,
    actor_id          TEXT,
    idempotency_key   TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL,
    completed_at      TIMESTAMPTZ,
    UNIQUE (tenant_id, idempotency_key)
);
CREATE INDEX idx_refunds_transaction ON refunds (transaction_id);
CREATE UNIQUE INDEX idx_refunds_reference ON refunds (gateway_reference) WHERE gateway_reference IS NOT NULL;

-- Webhooks seen, so redeliveries are acknowledged without effect.
CREATE TABLE webhook_events (
    gateway          TEXT NOT NULL,
    gateway_event_id TEXT NOT NULL,
    tenant_id        TEXT NOT NULL,
    event_type       TEXT NOT NULL,
    received_at      TIMESTAMPTZ NOT NULL,
    payload          JSONB NOT NULL,
    PRIMARY KEY (gateway, gateway_event_id)
);

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['tenant_gateway_config', 'transactions', 'refunds', 'webhook_events'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING (tenant_id = current_setting('app.tenant_id', true)
                   OR current_setting('app.tenant_id', true) = '_platform')
            WITH CHECK (tenant_id = current_setting('app.tenant_id', true))$p$, t);
    END LOOP;
END $$;
