-- Idempotency-Key storage for httpkit.Idempotency (api-conventions.md §10).
-- scope is tenant | caller | method | path; status is NULL while the first
-- request runs.
CREATE TABLE idempotency_keys (
    scope        TEXT NOT NULL,
    key          TEXT NOT NULL,
    request_hash BYTEA NOT NULL,
    status       INT,
    content_type TEXT,
    body         BYTEA,
    locked_until TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ,
    PRIMARY KEY (scope, key)
);
CREATE INDEX idx_idempotency_keys_created ON idempotency_keys (created_at);
