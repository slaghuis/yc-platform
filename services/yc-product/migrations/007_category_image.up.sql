-- One optional icon per category. The file lives in yc-image; this is its id.
ALTER TABLE categories ADD COLUMN image_id UUID;
