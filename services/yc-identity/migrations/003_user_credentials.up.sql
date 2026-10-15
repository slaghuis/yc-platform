-- Emails are stored lowercased so (tenant_id, email) uniqueness is
-- case-insensitive; the service normalises before writing.
ALTER TABLE users ADD CONSTRAINT users_email_lowercase CHECK (email = lower(email));

-- A user with credentials has an email.
ALTER TABLE users ADD CONSTRAINT users_credentials_have_email
    CHECK (password_hash IS NULL OR email IS NOT NULL);

-- GET /admin/users pages newest first.
CREATE INDEX idx_users_tenant_created ON users (tenant_id, created_at DESC, id DESC);
