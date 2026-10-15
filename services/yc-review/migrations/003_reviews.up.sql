-- Reviews, likes, product stats and review prompts
-- (docs/services/yc-review.md, Database Schema; ADR-024).

-- A shop's review settings. Shops without a row use the configured defaults.
CREATE TABLE review_settings (
    tenant_id        TEXT PRIMARY KEY,
    moderation_mode  TEXT NOT NULL CHECK (moderation_mode IN ('manual', 'auto')),
    require_purchase BOOLEAN NOT NULL,
    allow_edit       BOOLEAN NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- One review per customer per product. purchase_check_until is set while
-- yc-orders has not confirmed the purchase yet; the review stays pending
-- until then and is checked again at next_check_at.
CREATE TABLE reviews (
    id                   UUID PRIMARY KEY,
    tenant_id            TEXT NOT NULL,
    product_id           UUID NOT NULL,
    user_id              TEXT NOT NULL,
    rating               SMALLINT NOT NULL CHECK (rating BETWEEN 1 AND 5),
    title                TEXT NOT NULL DEFAULT '',
    body                 TEXT NOT NULL DEFAULT '',
    status               TEXT NOT NULL CHECK (status IN ('pending', 'approved', 'rejected')),
    rejection_reason     TEXT NOT NULL DEFAULT '',
    verified_purchase    BOOLEAN NOT NULL DEFAULT FALSE,
    purchase_order_id    UUID,
    purchase_check_until TIMESTAMPTZ,
    next_check_at        TIMESTAMPTZ,
    shop_response        TEXT NOT NULL DEFAULT '',
    shop_response_at     TIMESTAMPTZ,
    approved_at          TIMESTAMPTZ,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, product_id, user_id)
);
CREATE INDEX idx_reviews_product ON reviews (tenant_id, product_id, status, created_at DESC, id DESC);
CREATE INDEX idx_reviews_list ON reviews (tenant_id, status, created_at DESC, id DESC);
CREATE INDEX idx_reviews_user ON reviews (tenant_id, user_id, created_at DESC);
CREATE INDEX idx_reviews_check ON reviews (next_check_at) WHERE purchase_check_until IS NOT NULL;

CREATE TABLE product_likes (
    tenant_id  TEXT NOT NULL,
    product_id UUID NOT NULL,
    user_id    TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, product_id, user_id)
);
CREATE INDEX idx_product_likes_user ON product_likes (tenant_id, user_id, created_at DESC);

-- A product's approved-review and like counts, carried on review events.
-- version grows with every change so consumers keep the latest.
CREATE TABLE product_stats (
    tenant_id    TEXT NOT NULL,
    product_id   UUID NOT NULL,
    rating_count BIGINT NOT NULL DEFAULT 0,
    rating_sum   BIGINT NOT NULL DEFAULT 0,
    like_count   BIGINT NOT NULL DEFAULT 0,
    version      BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, product_id)
);

-- A delivered order whose customer is asked for reviews at due_at.
CREATE TABLE review_prompts (
    tenant_id  TEXT NOT NULL,
    order_id   UUID NOT NULL,
    user_id    TEXT NOT NULL,
    due_at     TIMESTAMPTZ NOT NULL,
    done_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, order_id)
);
CREATE INDEX idx_review_prompts_due ON review_prompts (due_at) WHERE done_at IS NULL;

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['review_settings', 'reviews', 'product_likes', 'product_stats', 'review_prompts'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING (tenant_id = current_setting('app.tenant_id', true)
                   OR current_setting('app.tenant_id', true) = '_platform')
            WITH CHECK (tenant_id = current_setting('app.tenant_id', true))$p$, t);
    END LOOP;
END $$;
