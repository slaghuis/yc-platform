-- Buyer billing snapshot on the checkout session until order create (issue #29).
ALTER TABLE checkout_sessions
    ADD COLUMN IF NOT EXISTS billing_address JSONB;
