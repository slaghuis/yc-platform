-- Parcel dimensions for carrier drivers, in millimetres: all or none.
ALTER TABLE skus
    ADD COLUMN length_mm INT CHECK (length_mm > 0),
    ADD COLUMN width_mm  INT CHECK (width_mm > 0),
    ADD COLUMN height_mm INT CHECK (height_mm > 0),
    ADD CONSTRAINT skus_dimensions_all_or_none CHECK (
        (length_mm IS NULL) = (width_mm IS NULL) AND (width_mm IS NULL) = (height_mm IS NULL));
