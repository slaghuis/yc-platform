-- Bound delivery window on the checkout session (issue #30).
ALTER TABLE checkout_sessions
    ADD COLUMN IF NOT EXISTS delivery_slot JSONB;
