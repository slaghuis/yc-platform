-- Phase 8: facts, aggregates and milestones (docs/services/yc-analytics.md,
-- ADR-025). Facts are the source of truth; aggregates are rebuilt from
-- them. Dates are UTC.

CREATE TABLE dim_tenant (
    tenant_id      TEXT PRIMARY KEY,
    name           TEXT NOT NULL,
    store_currency TEXT NOT NULL,
    provisioned_at TIMESTAMPTZ NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL
);

-- user_id is the buyer's ID, or their pseudonym after a GDPR deletion.
CREATE TABLE fact_order (
    order_id        UUID PRIMARY KEY,
    tenant_id       TEXT NOT NULL,
    date_key        DATE NOT NULL,
    order_number    TEXT NOT NULL,
    user_id         TEXT NOT NULL,
    user_type       TEXT NOT NULL,
    status          TEXT NOT NULL CHECK (status IN ('placed', 'cancelled')),
    item_count      INT NOT NULL,
    currency        TEXT NOT NULL,
    items_minor     BIGINT NOT NULL,
    tax_minor       BIGINT NOT NULL,
    shipping_minor  BIGINT NOT NULL,
    total_minor     BIGINT NOT NULL,
    refunded_minor  BIGINT NOT NULL DEFAULT 0,
    transaction_id  UUID,
    placed_at       TIMESTAMPTZ NOT NULL,
    cancelled_at    TIMESTAMPTZ,
    delivered_at    TIMESTAMPTZ
);
CREATE INDEX idx_fact_order_tenant_date ON fact_order (tenant_id, date_key);
CREATE INDEX idx_fact_order_user ON fact_order (tenant_id, user_id);

CREATE TABLE fact_order_item (
    order_id          UUID NOT NULL REFERENCES fact_order (order_id),
    line              INT NOT NULL,
    tenant_id         TEXT NOT NULL,
    date_key          DATE NOT NULL,
    sku_id            UUID NOT NULL,
    sku_code          TEXT NOT NULL,
    name              TEXT NOT NULL,
    quantity          BIGINT NOT NULL,
    currency          TEXT NOT NULL,
    unit_price_minor  BIGINT NOT NULL,
    line_total_minor  BIGINT NOT NULL,
    PRIMARY KEY (order_id, line)
);
CREATE INDEX idx_fact_order_item_tenant_date ON fact_order_item (tenant_id, date_key, sku_id);

-- One row per payment transaction; date_key is its first event's date.
CREATE TABLE fact_payment (
    transaction_id  UUID PRIMARY KEY,
    tenant_id       TEXT NOT NULL,
    date_key        DATE NOT NULL,
    gateway         TEXT NOT NULL,
    currency        TEXT NOT NULL,
    amount_minor    BIGINT NOT NULL,
    authorized_at   TIMESTAMPTZ,
    succeeded_at    TIMESTAMPTZ,
    failed_at       TIMESTAMPTZ,
    failure_reason  TEXT
);
CREATE INDEX idx_fact_payment_tenant_date ON fact_payment (tenant_id, date_key, gateway);

-- One row per checkout session and state it reached.
CREATE TABLE fact_checkout_step (
    session_id   UUID NOT NULL,
    state        TEXT NOT NULL CHECK (state IN ('started', 'confirmed', 'completed', 'failed', 'expired')),
    tenant_id    TEXT NOT NULL,
    date_key     DATE NOT NULL,
    occurred_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (session_id, state)
);
CREATE INDEX idx_fact_checkout_step_tenant_date ON fact_checkout_step (tenant_id, date_key, state);

CREATE TABLE fact_cart_event (
    cart_id      UUID NOT NULL,
    event        TEXT NOT NULL CHECK (event IN ('created', 'abandoned', 'converted')),
    tenant_id    TEXT NOT NULL,
    date_key     DATE NOT NULL,
    cart_type    TEXT NOT NULL,
    -- Whether the cart type counts in the funnel (convertible types).
    in_funnel    BOOLEAN NOT NULL,
    user_type    TEXT,
    occurred_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (cart_id, event)
);
CREATE INDEX idx_fact_cart_event_tenant_date ON fact_cart_event (tenant_id, date_key, event);

-- One row per visitor token issued (identity.visitor.created.v1); no
-- user reference is kept.
CREATE TABLE fact_visitor_session (
    event_id     UUID PRIMARY KEY,
    tenant_id    TEXT NOT NULL,
    date_key     DATE NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_fact_visitor_session_tenant_date ON fact_visitor_session (tenant_id, date_key);

CREATE TABLE agg_daily_revenue (
    tenant_id        TEXT NOT NULL,
    date_key         DATE NOT NULL,
    currency         TEXT NOT NULL,
    orders           BIGINT NOT NULL,
    cancelled        BIGINT NOT NULL,
    items_minor      BIGINT NOT NULL,
    tax_minor        BIGINT NOT NULL,
    shipping_minor   BIGINT NOT NULL,
    gross_minor      BIGINT NOT NULL,
    refunded_minor   BIGINT NOT NULL,
    net_minor        BIGINT NOT NULL,
    PRIMARY KEY (tenant_id, date_key, currency)
);

CREATE TABLE agg_daily_funnel (
    tenant_id            TEXT NOT NULL,
    date_key             DATE NOT NULL,
    visitors             BIGINT NOT NULL,
    carts_created        BIGINT NOT NULL,
    checkouts_started    BIGINT NOT NULL,
    checkouts_confirmed  BIGINT NOT NULL,
    orders_placed        BIGINT NOT NULL,
    PRIMARY KEY (tenant_id, date_key)
);

CREATE TABLE agg_product_performance (
    tenant_id      TEXT NOT NULL,
    date_key       DATE NOT NULL,
    sku_id         UUID NOT NULL,
    currency       TEXT NOT NULL,
    name           TEXT NOT NULL,
    units          BIGINT NOT NULL,
    revenue_minor  BIGINT NOT NULL,
    PRIMARY KEY (tenant_id, date_key, sku_id, currency)
);

CREATE TABLE agg_gateway_performance (
    tenant_id     TEXT NOT NULL,
    date_key      DATE NOT NULL,
    gateway       TEXT NOT NULL,
    currency      TEXT NOT NULL,
    attempts      BIGINT NOT NULL,
    successes     BIGINT NOT NULL,
    failures      BIGINT NOT NULL,
    amount_minor  BIGINT NOT NULL,
    PRIMARY KEY (tenant_id, date_key, gateway, currency)
);

-- Days whose facts changed since their aggregates were built.
CREATE TABLE agg_dirty (
    tenant_id  TEXT NOT NULL,
    date_key   DATE NOT NULL,
    marked_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, date_key)
);

-- Each milestone a shop reached, so it is announced once.
CREATE TABLE milestones (
    tenant_id   TEXT NOT NULL,
    milestone   TEXT NOT NULL,
    threshold   BIGINT NOT NULL,
    currency    TEXT NOT NULL DEFAULT '',
    order_id    UUID,
    reached_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, milestone, threshold, currency)
);

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['dim_tenant', 'fact_order', 'fact_order_item', 'fact_payment', 'fact_checkout_step',
        'fact_cart_event', 'fact_visitor_session', 'agg_daily_revenue', 'agg_daily_funnel',
        'agg_product_performance', 'agg_gateway_performance', 'agg_dirty', 'milestones']
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
            USING (tenant_id = current_setting('app.tenant_id', true)
                   OR current_setting('app.tenant_id', true) = '_platform')
            WITH CHECK (tenant_id = current_setting('app.tenant_id', true)
                        OR current_setting('app.tenant_id', true) = '_platform')$p$, t);
    END LOOP;
END $$;
