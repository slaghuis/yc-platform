-- Phase 12: invited system admins join the alert audience next to
-- admins.system_emails (ADR-030). Soft-suspend keeps the email so
-- reactivation can restore the audience without it on the event.
CREATE TABLE system_admins (
    user_id      TEXT NOT NULL PRIMARY KEY,
    email        TEXT NOT NULL,
    accepted_at  TIMESTAMPTZ NOT NULL,
    suspended_at TIMESTAMPTZ
);
