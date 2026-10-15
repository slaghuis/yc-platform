-- Checkout sessions and their saga state (docs/services/yc-checkout.md,
-- ADR-019). Amounts are integer minor units of the store currency.

CREATE TABLE checkout_sessions (
    id                      UUID PRIMARY KEY,
    tenant_id               TEXT NOT NULL,
    user_id                 TEXT NOT NULL,
    user_type               TEXT NOT NULL CHECK (user_type IN ('customer', 'visitor')),
    cart_type               TEXT NOT NULL,
    cart_id                 UUID,
    state                   TEXT NOT NULL CHECK (state IN ('PRICING', 'RESERVING', 'AUTHORIZING', 'ORDER_CREATE',
                                'CAPTURING', 'CONFIRMING', 'COMPLETED', 'COMPENSATING', 'FAILED', 'EXPIRED')),
    items                   JSONB NOT NULL,
    requires_shipping       BOOLEAN NOT NULL,
    has_tracked_items       BOOLEAN NOT NULL,
    currency                TEXT NOT NULL,
    tax_inclusive           BOOLEAN NOT NULL,
    items_total_minor       BIGINT NOT NULL,
    tax_total_minor         BIGINT NOT NULL,
    shipping_minor          BIGINT NOT NULL,
    total_minor             BIGINT NOT NULL,
    pricing_snapshot        JSONB NOT NULL,
    shipping_options        JSONB NOT NULL DEFAULT '[]',
    shipping_option_id      TEXT,
    guest_contact           JSONB,
    delivery_address        JSONB,
    -- The confirm request's Idempotency-Key; repeating it resumes the saga.
    confirm_key             TEXT,
    payment_gateway         TEXT,
    -- Transient: cleared once the authorization settles.
    payment_token           TEXT,
    return_url              TEXT,
    reservation_id          UUID,
    reservation_expires_at  TIMESTAMPTZ,
    payment_transaction_id  UUID,
    payment_status          TEXT,
    next_action             JSONB,
    order_id                UUID,
    order_number            TEXT,
    failed_state            TEXT,
    failure_reason          TEXT,
    failure_detail          JSONB,
    -- While COMPENSATING: FAILED or EXPIRED once compensation finishes.
    end_state               TEXT CHECK (end_state IN ('FAILED', 'EXPIRED')),
    compensation_log        JSONB NOT NULL DEFAULT '[]',
    attempts                INT NOT NULL DEFAULT 0,
    next_attempt_at         TIMESTAMPTZ,
    -- One replica drives a session at a time.
    lease_owner             TEXT,
    lease_until             TIMESTAMPTZ,
    expires_at              TIMESTAMPTZ NOT NULL,
    confirmed_at            TIMESTAMPTZ,
    completed_at            TIMESTAMPTZ,
    created_at              TIMESTAMPTZ NOT NULL,
    updated_at              TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_sessions_tenant_created ON checkout_sessions (tenant_id, created_at DESC, id DESC);
CREATE INDEX idx_sessions_tenant_user ON checkout_sessions (tenant_id, user_id);
CREATE INDEX idx_sessions_active ON checkout_sessions (state, expires_at)
    WHERE state NOT IN ('COMPLETED', 'FAILED', 'EXPIRED');

CREATE TABLE session_events (
    id          UUID PRIMARY KEY,
    tenant_id   TEXT NOT NULL,
    session_id  UUID NOT NULL REFERENCES checkout_sessions (id) ON DELETE CASCADE,
    event_type  TEXT NOT NULL,
    from_state  TEXT,
    to_state    TEXT,
    payload     JSONB,
    created_at  TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_session_events ON session_events (session_id, created_at);

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['checkout_sessions', 'session_events'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING (tenant_id = current_setting('app.tenant_id', true)
                   OR current_setting('app.tenant_id', true) = '_platform')
            WITH CHECK (tenant_id = current_setting('app.tenant_id', true)
                   OR current_setting('app.tenant_id', true) = '_platform')$p$, t);
    END LOOP;
END $$;
