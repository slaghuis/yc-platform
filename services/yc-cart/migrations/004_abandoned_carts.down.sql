DROP INDEX IF EXISTS idx_carts_abandoned;
DROP INDEX IF EXISTS idx_carts_idle;
ALTER TABLE carts DROP COLUMN IF EXISTS abandoned_at, DROP COLUMN IF EXISTS user_type;
