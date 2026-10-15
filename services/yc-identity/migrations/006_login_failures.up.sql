-- Failed password sign-ins per shop and email (Phase 9). Only a hash of the
-- normalised email is kept, and a row lasts until its window passes or the
-- next successful sign-in.
CREATE TABLE login_failures (
    tenant_id    TEXT NOT NULL,
    email_hash   BYTEA NOT NULL,
    failures     INT NOT NULL,
    window_start TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, email_hash)
);

CREATE INDEX login_failures_window ON login_failures (tenant_id, window_start);

ALTER TABLE login_failures ENABLE ROW LEVEL SECURITY;
ALTER TABLE login_failures FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON login_failures
    USING (tenant_id = current_setting('app.tenant_id', true)
           OR current_setting('app.tenant_id', true) = '_platform')
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
