ALTER TABLE preferences
    DROP COLUMN IF EXISTS newsletter,
    DROP COLUMN IF EXISTS marketing,
    DROP COLUMN IF EXISTS new_products;

ALTER TABLE preference_defaults
    DROP COLUMN IF EXISTS newsletter,
    DROP COLUMN IF EXISTS marketing,
    DROP COLUMN IF EXISTS new_products;
