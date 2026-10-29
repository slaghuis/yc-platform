-- Abandoned-cart recovery cadence (issue #35 / docs/features/Abandoned cart
-- recovery cadence and metrics.md). Fingerprint and items are snapshotted at
-- abandon; recovery_touch is the last touch sent (1..3); recovery_next_at is
-- when the next touch is due (NULL when the campaign is finished or cancelled).
ALTER TABLE carts
    ADD COLUMN recovery_fingerprint TEXT,
    ADD COLUMN recovery_items JSONB,
    ADD COLUMN recovery_touch INT NOT NULL DEFAULT 0
        CHECK (recovery_touch >= 0 AND recovery_touch <= 3),
    ADD COLUMN recovery_next_at TIMESTAMPTZ;

CREATE INDEX idx_carts_recovery_due ON carts (recovery_next_at)
    WHERE recovery_next_at IS NOT NULL AND abandoned_at IS NOT NULL;
