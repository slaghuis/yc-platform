DROP INDEX IF EXISTS idx_users_tenant_created;
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_credentials_have_email;
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_email_lowercase;
