-- Customer support threads (docs/features/Customer support contact threads.md).

CREATE TABLE support_requests (
    tenant_id        TEXT NOT NULL,
    id               UUID NOT NULL,
    reference        TEXT NOT NULL CHECK (reference ~ '^SUP-[A-Z0-9]{6}$'),
    topic            TEXT NOT NULL CHECK (topic IN ('allergen', 'delivery', 'order', 'payment', 'feedback', 'other')),
    status           TEXT NOT NULL CHECK (status IN ('new', 'open_customer', 'open_shop', 'closed')),
    -- Normalised (lower case, trimmed): the join key for visitors and claims.
    -- Emptied, with contact_name, when the requester is erased (GDPR).
    contact_email    TEXT NOT NULL,
    contact_name     TEXT NOT NULL,
    contact_phone    TEXT,
    -- NULL for a visitor until a customer claims the request.
    user_id          UUID,
    user_type        TEXT NOT NULL CHECK (user_type IN ('customer', 'visitor')),
    order_id         UUID,
    page_url         TEXT,
    last_activity_at TIMESTAMPTZ NOT NULL,
    closed_at        TIMESTAMPTZ,
    closed_reason    TEXT CHECK (closed_reason IN ('staff', 'idle')),
    redacted_at      TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, reference),
    -- Closed is terminal and says when and why; open requests have neither.
    CHECK ((status = 'closed') = (closed_at IS NOT NULL AND closed_reason IS NOT NULL)),
    -- Only customers have a user_id.
    CHECK (user_id IS NULL OR user_type = 'customer')
);

-- A customer's requests, most recently active first.
CREATE INDEX support_requests_user_idx ON support_requests (tenant_id, user_id, last_activity_at DESC, id DESC)
    WHERE user_id IS NOT NULL;
-- The admin list, with its status filter.
CREATE INDEX support_requests_activity_idx ON support_requests (tenant_id, last_activity_at DESC, id DESC);
CREATE INDEX support_requests_status_idx ON support_requests (tenant_id, status, last_activity_at DESC, id DESC);
-- Claims on registration match unclaimed requests by email.
CREATE INDEX support_requests_email_idx ON support_requests (tenant_id, contact_email)
    WHERE user_id IS NULL;
-- The idle sweeper, across shops.
CREATE INDEX support_requests_idle_idx ON support_requests (last_activity_at)
    WHERE status <> 'closed';

CREATE TABLE support_messages (
    tenant_id      TEXT NOT NULL,
    id             UUID NOT NULL,
    request_id     UUID NOT NULL,
    author_type    TEXT NOT NULL CHECK (author_type IN ('customer', 'visitor', 'shop_admin', 'system')),
    author_user_id UUID,
    -- Replaced by a placeholder when the requester is erased (GDPR).
    body           TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, request_id) REFERENCES support_requests (tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX support_messages_request_idx ON support_messages (tenant_id, request_id, created_at, id);

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['support_requests', 'support_messages'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING (tenant_id = current_setting('app.tenant_id', true)
                   OR current_setting('app.tenant_id', true) = '_platform')
            WITH CHECK (tenant_id = current_setting('app.tenant_id', true))$p$, t);
    END LOOP;
END $$;
