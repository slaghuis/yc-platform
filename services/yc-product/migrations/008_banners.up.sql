CREATE TABLE banners (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           TEXT NOT NULL,
    category_id         UUID NOT NULL,
    text                TEXT NOT NULL CHECK (text <> '' AND char_length(text) <= 280),
    background_image_id UUID NOT NULL,      -- yc-image
    large_image_id      UUID NOT NULL,      -- yc-image; not the category icon
    sort_order          INT NOT NULL DEFAULT 0,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, category_id),
    FOREIGN KEY (tenant_id, category_id) REFERENCES categories (tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX idx_banners_order ON banners (tenant_id, sort_order, created_at);

ALTER TABLE banners ENABLE ROW LEVEL SECURITY;
ALTER TABLE banners FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON banners
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform')
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
