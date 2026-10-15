-- Phase 5: the gateway is routed when the session starts (ADR-013), and
-- Variant B sessions pay in CHARGING.
ALTER TABLE checkout_sessions DROP CONSTRAINT checkout_sessions_state_check;
ALTER TABLE checkout_sessions ADD CONSTRAINT checkout_sessions_state_check CHECK (state IN ('PRICING', 'RESERVING',
    'AUTHORIZING', 'ORDER_CREATE', 'CHARGING', 'CAPTURING', 'CONFIRMING', 'COMPLETED', 'COMPENSATING', 'FAILED',
    'EXPIRED'));

ALTER TABLE checkout_sessions
    ADD COLUMN payment_variant    TEXT NOT NULL DEFAULT 'auth_capture' CHECK (payment_variant IN ('auth_capture', 'charge')),
    -- The customer pays on the gateway's page: no payment token.
    ADD COLUMN hosted_payment     BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN payment_public_key TEXT;
