-- Images (docs/services/yc-image.md, Database Schema).
--
-- status: pending (presigned upload not yet finalised) → processing →
-- ready | failed; any → deleted.

CREATE TABLE images (
    id              UUID PRIMARY KEY,
    tenant_id       TEXT NOT NULL,
    uploader_id     TEXT NOT NULL,
    status          TEXT NOT NULL CHECK (status IN ('pending', 'processing', 'ready', 'failed', 'deleted')),
    content_type    TEXT NOT NULL CHECK (content_type IN ('image/jpeg', 'image/png', 'image/webp', 'image/gif')),
    width           INT CHECK (width > 0),
    height          INT CHECK (height > 0),
    bytes           BIGINT CHECK (bytes > 0),
    checksum_sha256 TEXT CHECK (checksum_sha256 ~ '^[0-9a-f]{64}$'),
    owner_ref       TEXT CHECK (owner_ref ~ '^[a-z]+:[0-9a-f-]{36}$'),
    failure_reason  TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    processed_at    TIMESTAMPTZ,
    deleted_at      TIMESTAMPTZ,
    UNIQUE (tenant_id, id)
);
CREATE INDEX idx_images_tenant_created ON images (tenant_id, created_at DESC);
CREATE INDEX idx_images_owner_ref ON images (tenant_id, owner_ref) WHERE owner_ref IS NOT NULL;

CREATE TABLE image_variants (
    tenant_id   TEXT NOT NULL,
    image_id    UUID NOT NULL,
    variant     TEXT NOT NULL CHECK (variant IN ('original', 'xlarge', 'large', 'medium', 'thumbnail')),
    format      TEXT NOT NULL CHECK (format IN ('jpeg', 'png', 'webp')),
    width       INT NOT NULL CHECK (width > 0),
    height      INT NOT NULL CHECK (height > 0),
    bytes       BIGINT NOT NULL CHECK (bytes > 0),
    storage_key TEXT NOT NULL,
    url         TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (image_id, variant, format),
    FOREIGN KEY (tenant_id, image_id) REFERENCES images (tenant_id, id) ON DELETE CASCADE
);

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['images', 'image_variants'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING (tenant_id = current_setting('app.tenant_id', true)
                   OR current_setting('app.tenant_id', true) = '_platform')
            WITH CHECK (tenant_id = current_setting('app.tenant_id', true))$p$, t);
    END LOOP;
END $$;
