-- Abandoned carts (docs/services/yc-cart.md, Abandoned Carts). user_type
-- becomes customer once a customer uses the cart; abandoned_at is set when
-- cart.abandoned.v1 is published and cleared by the next activity, so a
-- cart is announced once per idle spell.
ALTER TABLE carts
    ADD COLUMN user_type TEXT NOT NULL DEFAULT 'visitor' CHECK (user_type IN ('visitor', 'customer')),
    ADD COLUMN abandoned_at TIMESTAMPTZ;

CREATE INDEX idx_carts_idle ON carts (updated_at) WHERE user_type = 'customer' AND abandoned_at IS NULL;
CREATE INDEX idx_carts_abandoned ON carts (tenant_id, abandoned_at DESC, id DESC) WHERE abandoned_at IS NOT NULL;
