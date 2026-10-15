ALTER TABLE checkout_sessions
    DROP COLUMN payment_public_key,
    DROP COLUMN hosted_payment,
    DROP COLUMN payment_variant;

ALTER TABLE checkout_sessions DROP CONSTRAINT checkout_sessions_state_check;
ALTER TABLE checkout_sessions ADD CONSTRAINT checkout_sessions_state_check CHECK (state IN ('PRICING', 'RESERVING',
    'AUTHORIZING', 'ORDER_CREATE', 'CAPTURING', 'CONFIRMING', 'COMPLETED', 'COMPENSATING', 'FAILED', 'EXPIRED'));
