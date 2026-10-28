-- Bound address-book id on the checkout session (issue #32).
ALTER TABLE checkout_sessions
    ADD COLUMN IF NOT EXISTS delivery_address_id UUID;
