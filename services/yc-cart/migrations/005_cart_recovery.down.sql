DROP INDEX IF EXISTS idx_carts_recovery_due;
ALTER TABLE carts
    DROP COLUMN IF EXISTS recovery_next_at,
    DROP COLUMN IF EXISTS recovery_touch,
    DROP COLUMN IF EXISTS recovery_items,
    DROP COLUMN IF EXISTS recovery_fingerprint;
