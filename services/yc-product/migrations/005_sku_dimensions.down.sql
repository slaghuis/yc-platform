ALTER TABLE skus
    DROP CONSTRAINT IF EXISTS skus_dimensions_all_or_none,
    DROP COLUMN IF EXISTS length_mm,
    DROP COLUMN IF EXISTS width_mm,
    DROP COLUMN IF EXISTS height_mm;
